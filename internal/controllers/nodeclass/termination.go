package nodeclass

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

// terminationBlockedRequeue is how often a NodeClass held open by live
// NodeClaims re-checks and re-publishes its event. The wait itself is
// indefinite, so this only controls how often the explanation is refreshed.
const terminationBlockedRequeue = 10 * time.Minute

// Termination releases an HCloudNodeClass once nothing references it.
//
// Provider-owned entirely: core declares no NodeClass finalizer, ships no
// termination controller and does not cascade, so upstream a deleted NodeClass
// leaves its live NodeClaims pointing at nothing while their nodes keep running,
// drifting and consolidating against a class that is gone. Nothing Hetzner-side
// needs cleaning up (networks, firewalls, ssh keys and placement groups are
// referenced, never owned), so the finalizer's whole job is the ordering gate.
type Termination struct {
	kubeClient client.Client
	recorder   events.Recorder
}

// NewTermination returns the termination reconciler.
func NewTermination(kubeClient client.Client, recorder events.Recorder) *Termination {
	return &Termination{kubeClient: kubeClient, recorder: recorder}
}

// Finalize releases the NodeClass if no NodeClaim still references it.
func (t *Termination) Finalize(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	stored := nodeClass.DeepCopy()
	if !controllerutil.ContainsFinalizer(nodeClass, v1alpha1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}

	// An unregistered field index ERRORS rather than returning an empty list, so
	// a missing index cannot make this release the finalizer while NodeClaims
	// still exist. The hazard is the opposite one: core's setupIndexers fails
	// OPEN when the NodeClaim CRD is absent at operator start, and indexes
	// register only at manager construction, so this List then errors forever
	// and a deleted HCloudNodeClass hangs until the operator is restarted.
	nodeClaims := &karpv1.NodeClaimList{}
	if err := t.kubeClient.List(ctx, nodeClaims, nodeclaimutils.ForNodeClass(nodeClass)); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing nodeclaims using nodeclass, %w", err)
	}
	if len(nodeClaims.Items) > 0 {
		names := make([]string, 0, len(nodeClaims.Items))
		for i := range nodeClaims.Items {
			names = append(names, nodeClaims.Items[i].Name)
		}
		// Sorted so the event message does not change with list order.
		sort.Strings(names)
		t.recorder.Publish(WaitingOnNodeClaimTerminationEvent(nodeClass, names))
		// Blocks indefinitely, on purpose: releasing the class while nodes
		// still reference it does not delete those nodes, it only removes the
		// record of how they were built.
		return reconcile.Result{RequeueAfter: terminationBlockedRequeue}, nil
	}

	controllerutil.RemoveFinalizer(nodeClass, v1alpha1.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		// MergeFromWithOptimisticLock because a JSON merge patch replaces the
		// finalizer list wholesale, so without the resourceVersion precondition
		// a concurrent writer's finalizer is dropped rather than merged.
		if err := t.kubeClient.Patch(ctx, nodeClass, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return reconcile.Result{RequeueAfter: conflictRequeue}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(fmt.Errorf("removing termination finalizer, %w", err))
		}
	}
	return reconcile.Result{}, nil
}
