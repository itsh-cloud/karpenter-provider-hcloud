// Package nodeclass reconciles HCloudNodeClass objects. It resolves every
// selector on the spec against the Hetzner API, publishes the resolved
// identifiers in status, and rolls the per-resource conditions up into Ready,
// which is what karpenter core gates provisioning on.
package nodeclass

import (
	"context"
	"time"

	"go.uber.org/multierr"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/utils/result"

	"github.com/awslabs/operatorpkg/status"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// conflictRequeue is the delay after losing an optimistic-lock race. A conflict
// only means someone else wrote the object, so the fix is to re-read it; the
// short delay stops two controllers spinning against each other.
const conflictRequeue = time.Second

// ControllerName is how this controller identifies itself in logs and metrics.
const ControllerName = "nodeclass"

// Controller resolves an HCloudNodeClass and reports the result in its status.
//
// Sub-reconcilers mutate only the in-memory object and never patch. The parent
// owns the single status write, so one pass persists every observation at once
// and the Ready roll-up settles in one step.
type Controller struct {
	kubeClient  client.Client
	termination *Termination
	reconcilers []reconcile.TypedReconciler[*v1alpha1.HCloudNodeClass]
	// catalogRefreshed wakes every NodeClass when the server type catalog
	// lands. Optional: nil means no wake-up, and validation's slow backstop
	// requeue still converges.
	catalogRefreshed <-chan struct{}
}

// NewController returns the HCloudNodeClass status controller.
func NewController(
	clk clock.Clock,
	kubeClient client.Client,
	recorder events.Recorder,
	resources hcloudapi.Resources,
	catalogProvider CatalogProvider,
	discovery Discovery,
) *Controller {
	// The wake-up is an optimisation, not a dependency, and construction must
	// not panic on a partially wired provider: that happens at operator startup,
	// where a panic is a CrashLoopBackOff with no useful message.
	var catalogRefreshed <-chan struct{}
	if catalogProvider != nil {
		catalogRefreshed = catalogProvider.Refreshed()
	}

	return &Controller{
		kubeClient:       kubeClient,
		termination:      NewTermination(kubeClient, recorder),
		catalogRefreshed: catalogRefreshed,
		reconcilers: []reconcile.TypedReconciler[*v1alpha1.HCloudNodeClass]{
			NewImage(clk, resources),
			NewNetwork(clk, resources),
			NewFirewalls(clk, resources),
			NewSSHKeys(clk, recorder, resources),
			NewPlacementGroup(clk, resources),
			NewBootstrapDiscovery(clk, discovery),
			// Last, and the order is load-bearing: validation reports on the
			// six conditions above and reads the zone the network reconciler
			// just resolved.
			NewValidation(clk, recorder, catalogProvider),
		},
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, ControllerName)

	if !nodeClass.GetDeletionTimestamp().IsZero() {
		return c.termination.Finalize(ctx, nodeClass)
	}

	if !controllerutil.ContainsFinalizer(nodeClass, v1alpha1.TerminationFinalizer) {
		stored := nodeClass.DeepCopy()
		controllerutil.AddFinalizer(nodeClass, v1alpha1.TerminationFinalizer)
		// Separate from the status patch below, necessarily: metadata and the
		// status subresource are different endpoints. Optimistic lock because a
		// JSON merge patch replaces the finalizer list wholesale.
		if err := c.kubeClient.Patch(ctx, nodeClass, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(err) != nil {
			if apierrors.IsConflict(err) {
				return reconcile.Result{RequeueAfter: conflictRequeue}, nil
			}
			return reconcile.Result{}, err
		}
	}

	// Snapshotted AFTER the finalizer patch, so that metadata change is not
	// re-sent in the status patch, and BEFORE the first StatusConditions() call,
	// which is a constructor: it initialises absent conditions to Unknown in
	// memory, and a later snapshot would hide that from the diff guard below,
	// leaving the object with no conditions at all.
	stored := nodeClass.DeepCopy()

	var results []reconcile.Result
	var errs error
	for _, reconciler := range c.reconcilers {
		// Every sub-reconciler runs on every pass, even after one fails.
		// Skipping the rest leaves a stale observedGeneration on their
		// conditions, which the roll-up counts as unhealthy, pinning Ready at
		// Unknown even once the failure clears.
		res, err := reconciler.Reconcile(ctx, nodeClass)
		errs = multierr.Append(errs, err)
		results = append(results, res)
	}

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := c.kubeClient.Status().Patch(ctx, nodeClass, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			// Short-circuit only when nothing else went wrong. Taking it with
			// errs non-empty discards a coincident Hetzner failure entirely,
			// unlogged and uncounted, because the pass then looks successful.
			if apierrors.IsConflict(err) && errs == nil {
				return reconcile.Result{RequeueAfter: conflictRequeue}, nil
			}
			errs = multierr.Append(errs, client.IgnoreNotFound(err))
		}
	}
	if errs != nil {
		return reconcile.Result{}, errs
	}
	return result.Min(results...), nil
}

// rateLimiter paces retries after a failed pass.
//
// Deliberately NOT operatorpkg's reasonable.RateLimiter, whose 100ms base is an
// amplifier here: one pass is five to eighteen uncached Hetzner GETs, against a
// 3600/hour per-project limit shared with the CCM and the CSI driver, and
// rate_limit_exceeded classifies transient and is retried. Five seconds to two
// minutes removes that and costs a few seconds on a genuine blip.
//
// ERROR path only: a RequeueAfter with a nil error goes through Queue.Forget and
// AddAfter, which consult neither the limiter nor the bucket.
func rateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Second, 2*time.Minute),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// Register wires the controller into the manager.
func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	b := controllerruntime.NewControllerManagedBy(m).
		Named(ControllerName).
		For(&v1alpha1.HCloudNodeClass{}).
		Watches(
			&karpv1.NodeClaim{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				nc, ok := o.(*karpv1.NodeClaim)
				if !ok || nc.Spec.NodeClassRef == nil {
					return nil
				}
				// Group and Kind, not just the name: another provider's
				// NodeClass may share a name, and enqueueing on it spends a
				// full seven-GET resolve for nothing.
				ref := nc.Spec.NodeClassRef
				if ref.Group != v1alpha1.Group || ref.Kind != "HCloudNodeClass" {
					return nil
				}
				// The watch exists only to release the finalizer as soon as the
				// last NodeClaim goes, instead of waiting out the ten-minute
				// requeue. On a class that is not terminating the enqueue buys a
				// full resolve for nothing, and this read is served from the
				// informer cache.
				key := types.NamespacedName{Name: ref.Name}
				nodeClass := &v1alpha1.HCloudNodeClass{}
				if err := m.GetClient().Get(ctx, key, nodeClass); err != nil || nodeClass.GetDeletionTimestamp().IsZero() {
					return nil
				}
				return []reconcile.Request{{NamespacedName: key}}
			}),
			// Deletes only: a NodeClaim changing says nothing about whether its
			// class resolves. The cost is that
			// status.placementGroup.serverCount refreshes only on the
			// five-minute requeue, so the near-capacity warning can lag by that
			// much. Cheaper than seven Hetzner GETs per node launched, on every
			// class, for one advisory condition on an opt-in field.
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(event.CreateEvent) bool { return false },
				UpdateFunc:  func(event.UpdateEvent) bool { return false },
				DeleteFunc:  func(event.DeleteEvent) bool { return true },
				GenericFunc: func(event.GenericEvent) bool { return false },
			}),
		).
		WithOptions(controller.Options{
			RateLimiter:             rateLimiter(),
			MaxConcurrentReconciles: 10,
		})

	// Waking on the catalog rather than polling it. Validation cannot succeed
	// without the server type catalog, and the catalog is refreshed by a
	// Runnable in this same process, so a landing snapshot enqueues the
	// NodeClasses waiting on it and the requeue stays a slow backstop.
	if c.catalogRefreshed != nil {
		events := make(chan event.GenericEvent)
		go pumpRefreshSignal(ctx, c.catalogRefreshed, events)
		b = b.WatchesRawSource(source.Channel(events, handler.EnqueueRequestsFromMapFunc(enqueueCatalogWaiters(m.GetClient()))))
	}

	return b.Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// pumpRefreshSignal forwards catalog wake-ups onto the watch channel.
//
// Not the catalog's channel handed straight to source.Channel: the catalog's
// send is non-blocking, so a refresh is never delayed by a listener, while
// source.Channel's reader does not start until the manager does. Parking here
// absorbs that gap.
func pumpRefreshSignal(ctx context.Context, src <-chan struct{}, dst chan<- event.GenericEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-src:
			select {
			case dst <- event.GenericEvent{}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// enqueueCatalogWaiters returns a map func enqueueing only the NodeClasses
// blocked on the catalog.
//
// The signal carries no object and the catalog refreshes roughly six times an
// hour, so fanning out would cost every class a full five-GET pass for nothing:
// a class that is already Ready re-validates on its own requeue regardless.
func enqueueCatalogWaiters(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		list := &v1alpha1.HCloudNodeClassList{}
		if err := c.List(ctx, list); err != nil {
			log.FromContext(ctx).Error(err, "listing HCloudNodeClasses after a catalog refresh")
			return nil
		}
		var reqs []reconcile.Request
		for i := range list.Items {
			if waitingOnCatalog(&list.Items[i]) {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
			}
		}
		return reqs
	}
}

// waitingOnCatalog reports whether this NodeClass is parked on the catalog.
//
// WithObservedOnly because StatusConditions is a constructor: the plain form
// fabricates every absent condition as Unknown on the in-memory copy, which
// would answer from what it just invented rather than from the object.
func waitingOnCatalog(nodeClass *v1alpha1.HCloudNodeClass) bool {
	cond := nodeClass.StatusConditions(status.WithObservedOnly()).Get(v1alpha1.ConditionTypeValidationSucceeded)
	if cond == nil {
		// Never reconciled. It needs the catalog and has not said so yet.
		return true
	}
	return cond.IsUnknown() && (cond.Reason == reasonCatalogNotFetched || cond.Reason == reasonCatalogEmpty)
}
