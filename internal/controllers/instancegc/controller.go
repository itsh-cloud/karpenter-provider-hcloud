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
	// orphan.
	//
	// This is the entire safety margin of the controller. A server is created
	// BEFORE its NodeClaim records the providerID, so during that window a
	// perfectly healthy new server legitimately has no NodeClaim pointing at
	// it. Reaping inside that window would delete the node it was asked to
	// build, repeatedly. Five minutes is comfortably beyond a create plus its
	// action wait, and well under the point where a leaked server costs real
	// money.
	minAge = 5 * time.Minute
)

// Provider is the slice of the instance provider this controller needs.
type Provider interface {
	List(ctx context.Context) ([]*hcloudapi.Server, error)
	Delete(ctx context.Context, providerID string) error
}

// Controller deletes servers whose NodeClaim is gone.
//
// # Why this has to exist
//
// Karpenter core ships the mirror of this and not this. Its
// nodeclaim/garbagecollection controller deletes NODECLAIMS whose instance has
// vanished, which is the leak in the other direction. Nothing in core deletes
// an INSTANCE whose NodeClaim has vanished, because only the provider can
// enumerate its own instances. Every provider ships its own; without it a
// server can outlive the object that knows about it.
//
// The leak is not hypothetical. A create can succeed at Hetzner and then fail
// to be recorded: the response is lost, the cleanup delete also fails, or the
// process dies between the two. The server is running, it booted with a valid
// join token so it will register as a Node, and it bills indefinitely with
// nothing referring to it.
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
		// Unreachable in a wired binary, which refuses to start without it.
		// Checked anyway because the consequence of being wrong here is
		// deleting servers this cluster does not own.
		return reconciler.Result{}, fmt.Errorf("cluster name is empty, refusing to garbage collect")
	}

	servers, err := c.provider.List(ctx)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("listing servers, %w", err)
	}
	if len(servers) == 0 {
		return reconciler.Result{RequeueAfter: interval}, nil
	}

	// Every NodeClaim, not just those with a providerID.
	//
	// A NodeClaim that has been created but has not yet recorded its
	// providerID is exactly the in-flight case this must not reap, so the set
	// is keyed by NAME, which the server carries as a label from the moment it
	// is created. Keying on providerID would leave a window where a launching
	// NodeClaim looks like an orphan.
	nodeClaims := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaims); err != nil {
		return reconciler.Result{}, fmt.Errorf("listing nodeclaims, %w", err)
	}
	live := sets.New[string]()
	for i := range nodeClaims.Items {
		live.Insert(nodeClaims.Items[i].Name)
	}

	// No NodeClaims at all, but servers that say they belong to some, is not a
	// fleet of orphans. It is what a lost list, a wiped CRD, or somebody's
	// `kubectl delete nodeclaims --all` looks like from here, and acting on it
	// deletes every node this cluster runs, undrained, in one pass.
	//
	// A genuine leak is one or a few servers against a populated list, which
	// this still reaps on the next sweep two minutes later. Refusing the
	// all-or-nothing shape costs that case nothing and takes the worst outcome
	// off the table entirely.
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
		// A missing creation time reads as an age of ~2.5 million hours, which
		// would reap instantly. Unknown age means "cannot judge", and on a path
		// that deletes machines that has to resolve to leaving it alone.
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

		metrics.OrphansReaped.Inc()
		if err := c.provider.Delete(ctx, srv.ProviderID()); err != nil {
			if hcloudapi.IsNotFound(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("deleting orphaned server %s, %w", srv.Name, err))
		}
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
