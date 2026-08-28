// Package instancegc removes Hetzner servers this provider created that no
// longer have a NodeClaim behind them.
package instancegc

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/metrics"
)

// ControllerName is how this controller identifies itself in logs and metrics.
const ControllerName = "instance.garbagecollection"

const (
	// interval is how often the orphan sweep runs. Orphans are rare and cost
	// money rather than correctness, so this trades promptness for a share of
	// the Hetzner rate limit.
	interval = 2 * time.Minute

	// minAge is how long a server must exist before it can be considered an
	// orphan, and it is the controller's entire safety margin. A server is
	// created BEFORE its NodeClaim records the providerID, so reaping inside
	// that window would repeatedly delete the node it was asked to build. Five
	// minutes is well beyond a create plus its action wait.
	minAge = 5 * time.Minute
)

// Provider is the slice of the instance provider this controller needs.
type Provider interface {
	List(ctx context.Context) ([]*hcloudapi.Server, error)
	Delete(ctx context.Context, providerID string) error
}

// Controller deletes servers whose NodeClaim is gone.
//
// Core ships only the mirror of this: it reaps NODECLAIMS whose instance has
// vanished, never instances whose NodeClaim has, because only a provider can
// enumerate its own instances. The leak is reachable whenever a create succeeds
// at Hetzner and fails to be recorded (lost response, failed cleanup delete,
// process death between the two): the server runs, registers as a Node on its
// valid join token, and bills with nothing referring to it.
type Controller struct {
	kubeClient  client.Client
	provider    Provider
	clock       clock.Clock
	clusterName string
}

// NewController returns the instance garbage collector.
func NewController(clk clock.Clock, kubeClient client.Client, provider Provider, clusterName string) *Controller {
	return &Controller{kubeClient: kubeClient, provider: provider, clock: clk, clusterName: clusterName}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, ControllerName)

	if c.clusterName == "" {
		// Unreachable in a wired binary, which refuses to start without it,
		// but the consequence of being wrong is deleting other people's
		// servers.
		return reconciler.Result{}, fmt.Errorf("cluster name is empty, refusing to garbage collect")
	}

	servers, err := c.provider.List(ctx)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("listing servers, %w", err)
	}
	if len(servers) == 0 {
		return reconciler.Result{RequeueAfter: interval}, nil
	}

	// Every NodeClaim, and keyed by NAME, which the server carries as a label
	// from the moment it is created. Keying on providerID would leave a window
	// where a NodeClaim that has not yet recorded one looks like an orphan.
	nodeClaims := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaims); err != nil {
		return reconciler.Result{}, fmt.Errorf("listing nodeclaims, %w", err)
	}
	live := sets.New[string]()
	for i := range nodeClaims.Items {
		live.Insert(nodeClaims.Items[i].Name)
	}

	// No NodeClaims at all, against servers that claim to have some, is not a
	// fleet of orphans: it is what a lost list, a wiped CRD or a
	// `kubectl delete nodeclaims --all` looks like from here, and acting on it
	// deletes every node this cluster runs, undrained, in one pass. A genuine
	// leak is a few servers against a populated list, still reaped next sweep.
	if len(live) == 0 {
		log.FromContext(ctx).Info("refusing to garbage collect: there are no NodeClaims at all, "+
			"which is indistinguishable from having lost them rather than from every server being an orphan",
			"servers", len(servers))
		return reconciler.Result{RequeueAfter: interval}, nil
	}

	var errs []error
	for _, srv := range servers {
		claim := srv.Labels[hcloudapi.LabelNodeClaim]
		if claim == "" || live.Has(claim) {
			continue
		}
		// A missing creation time reads as an age of ~2.5 million hours and
		// would reap instantly. On a path that deletes machines, an age that
		// cannot be judged has to resolve to leaving the server alone.
		if srv.Created.IsZero() {
			log.FromContext(ctx).Info("skipping a server with no creation time; its age cannot be judged",
				"server", srv.Name, "id", srv.ID)
			continue
		}
		if age := c.clock.Since(srv.Created); age < minAge {
			// Too young to judge: it may be mid-create, with its NodeClaim
			// about to appear.
			continue
		}

		log.FromContext(ctx).Info("deleting an orphaned server: it carries this cluster's label but its NodeClaim is gone",
			"server", srv.Name, "id", srv.ID, "nodeclaim", claim, "age", c.clock.Since(srv.Created).Truncate(time.Second).String())

		if err := c.provider.Delete(ctx, srv.ProviderID()); err != nil {
			if hcloudapi.IsNotFound(err) {
				// Somebody else removed it. Not a reap by us, so not counted.
				continue
			}
			errs = append(errs, fmt.Errorf("deleting orphaned server %s, %w", srv.Name, err))
			continue
		}
		// Counted only after the delete succeeded: incrementing before would
		// re-count on every requeue of a failing delete, turning one stuck
		// orphan into a rising rate that looks like many leaks.
		metrics.OrphansReaped.Inc()
	}
	if len(errs) > 0 {
		return reconciler.Result{}, fmt.Errorf("garbage collecting orphaned servers: %w", multierr.Combine(errs...))
	}
	return reconciler.Result{RequeueAfter: interval}, nil
}

// Register wires the controller into the manager.
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(ControllerName).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
