// Package hash maintains the HCloudNodeClass spec hash that drift compares
// against, and back-fills it across NodeClaims when the hash generator changes.
package hash

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

// ControllerName is how this controller identifies itself in logs and metrics.
const ControllerName = "nodeclass.hash"

// Controller stamps the current spec hash onto the HCloudNodeClass.
//
// Separate from the status controller because it writes METADATA rather than
// the status subresource: folding it into that controller's single status patch
// would silently drop the annotation.
type Controller struct {
	kubeClient client.Client
}

// NewController returns the hash controller.
func NewController(kubeClient client.Client) *Controller {
	return &Controller{kubeClient: kubeClient}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, ControllerName)

	// A terminating class will never launch another node, so its hash is dead
	// weight and writing it only races the finalizer.
	if !nodeClass.GetDeletionTimestamp().IsZero() {
		return reconcile.Result{}, nil
	}

	stored := nodeClass.DeepCopy()

	// The back-fill runs BEFORE the class's own annotation is rewritten, and
	// that order is what stops a controller upgrade from replacing the fleet: a
	// changed generator produces a different hash for an unchanged spec, so
	// re-stamping the class first would leave every NodeClaim holding an old
	// hash against a new one, which drift reads as "every node is out of date".
	if nodeClass.Annotations[v1alpha1.AnnotationHashVersion] != v1alpha1.HashVersion {
		if err := c.backfillNodeClaimHashes(ctx, nodeClass); err != nil {
			return reconcile.Result{}, err
		}
	}

	nodeClass.Annotations = lo.Assign(nodeClass.Annotations, map[string]string{
		v1alpha1.AnnotationHash:        nodeClass.Hash(),
		v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion,
	})

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := c.kubeClient.Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
}

// backfillNodeClaimHashes re-stamps every NodeClaim of this class with the hash
// the current generator produces, so that like is compared with like once the
// version annotation advances.
func (c *Controller) backfillNodeClaimHashes(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) error {
	nodeClaims := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaims, nodeclaimutils.ForNodeClass(nodeClass)); err != nil {
		return fmt.Errorf("listing nodeclaims using nodeclass, %w", err)
	}

	hash := nodeClass.Hash()
	errs := make([]error, len(nodeClaims.Items))
	for i := range nodeClaims.Items {
		nc := &nodeClaims.Items[i]
		if nc.Annotations[v1alpha1.AnnotationHashVersion] == v1alpha1.HashVersion {
			continue
		}
		stored := nc.DeepCopy()
		updates := map[string]string{v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion}

		// An already-drifted NodeClaim keeps its stale hash: it is condemned
		// already, and re-stamping would un-drift it with no way to tell whether
		// the reason still applies. Cancelling a replacement is the
		// unrecoverable direction to guess wrong in.
		//
		// WithObservedOnly because StatusConditions is a constructor: the plain
		// form fabricates conditions as Unknown on the in-memory copy, which
		// defeats the diff guard below and sends a patch that was not needed.
		if nc.StatusConditions(status.WithObservedOnly()).Get(karpv1.ConditionTypeDrifted) == nil {
			updates[v1alpha1.AnnotationHash] = hash
		}
		nc.Annotations = lo.Assign(nc.Annotations, updates)

		if !equality.Semantic.DeepEqual(stored, nc) {
			if err := c.kubeClient.Patch(ctx, nc, client.MergeFrom(stored)); err != nil {
				errs[i] = client.IgnoreNotFound(err)
			}
		}
	}
	return multierr.Combine(errs...)
}

// Register wires the controller into the manager.
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(ControllerName).
		For(&v1alpha1.HCloudNodeClass{}).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
