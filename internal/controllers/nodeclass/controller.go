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
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/utils/result"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// conflictRequeue is the delay after losing an optimistic-lock race.
//
// A conflict is not an error: someone else wrote the object a moment ago, and
// the fix is to re-read it. reconcile.Result.Requeue, which upstream uses for
// this, is deprecated in controller-runtime, and requeueing with no delay at
// all invites two controllers to spin against each other, so this waits a beat.
const conflictRequeue = time.Second

// ControllerName is how this controller identifies itself in logs and metrics.
const ControllerName = "nodeclass"

// Controller resolves an HCloudNodeClass and reports the result in its status.
//
// The work is split into typed sub-reconcilers that mutate only the in-memory
// object and never patch. The parent owns the one status write, so a pass that
// resolves six things and fails the seventh persists all seven observations in
// a single update instead of seven, and the Ready roll-up settles in one step
// rather than converging over several reconciles.
type Controller struct {
	kubeClient  client.Client
	termination *Termination
	reconcilers []reconcile.TypedReconciler[*v1alpha1.HCloudNodeClass]
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
	return &Controller{
		kubeClient:  kubeClient,
		termination: NewTermination(kubeClient, recorder),
		reconcilers: []reconcile.TypedReconciler[*v1alpha1.HCloudNodeClass]{
			NewImage(clk, resources),
			NewNetwork(clk, resources),
			NewFirewalls(clk, resources),
			NewSSHKeys(clk, recorder, resources),
			NewPlacementGroup(clk, resources),
			NewBootstrapDiscovery(clk, discovery),
			// Last, and the order is load-bearing: validation reports on the
			// six conditions above and reads the network zone the network
			// reconciler just resolved.
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
		// A separate patch from the status one below, and it has to be: this
		// writes metadata, that writes the status subresource, and the two are
		// different endpoints. MergeFromWithOptimisticLock because a JSON merge
		// patch replaces the finalizer list wholesale.
		if err := c.kubeClient.Patch(ctx, nodeClass, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(err) != nil {
			if apierrors.IsConflict(err) {
				return reconcile.Result{RequeueAfter: conflictRequeue}, nil
			}
			return reconcile.Result{}, err
		}
	}

	// Snapshotted here, AFTER the finalizer patch and BEFORE any sub-reconciler
	// runs. The order matters in both directions. After the finalizer patch, so
	// the metadata change is not re-sent as part of the status patch. Before the
	// first StatusConditions() call, because that call is a constructor rather
	// than an accessor: it initialises every absent condition to Unknown in
	// memory, and a snapshot taken afterwards would hide that initialisation
	// from the diff guard below, leaving the object with no conditions at all.
	stored := nodeClass.DeepCopy()

	var results []reconcile.Result
	var errs error
	for _, reconciler := range c.reconcilers {
		// Every sub-reconciler runs on every pass, including after one has
		// failed. Skipping the rest would leave their conditions carrying a
		// stale observedGeneration, which the roll-up counts as unhealthy, so
		// Ready would be pinned Unknown even once the failure cleared.
		res, err := reconciler.Reconcile(ctx, nodeClass)
		errs = multierr.Append(errs, err)
		results = append(results, res)
	}

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := c.kubeClient.Status().Patch(ctx, nodeClass, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			// The conflict short-circuit is taken only when nothing else went
			// wrong. Returning it while errs is non-empty discards a coincident
			// Hetzner failure entirely: never logged, never counted in
			// controller_runtime_reconcile_errors_total, and invisible because
			// the pass looks like it succeeded.
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
// Deliberately NOT operatorpkg's reasonable.RateLimiter, which starts at 100ms.
// One pass here is up to seven uncached Hetzner GETs, so a 100ms base means a
// single failing NodeClass issues roughly eleven passes, ~77 requests, inside
// the first hundred seconds of an outage, and the accompanying 10qps/burst-100
// token bucket lets ten of them do that at once. Against a 3600/hour
// per-project limit shared with the CCM and the CSI driver, that makes being
// rate limited cause more requests than being healthy, since rate_limit_exceeded
// is classified transient and therefore retried.
//
// Five seconds to two minutes costs a few seconds of extra latency on a genuine
// blip and removes the amplifier. The bucket is kept as a ceiling on the whole
// controller.
func rateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Second, 2*time.Minute),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// Register wires the controller into the manager.
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(ControllerName).
		For(&v1alpha1.HCloudNodeClass{}).
		Watches(
			&karpv1.NodeClaim{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				nc, ok := o.(*karpv1.NodeClaim)
				if !ok || nc.Spec.NodeClassRef == nil {
					return nil
				}
				// Group and Kind are matched, not just the name. Another
				// provider's NodeClass may share a name, and enqueueing on it
				// would spend a full seven-GET resolve on an object that has
				// nothing to do with this NodeClaim.
				ref := nc.Spec.NodeClassRef
				if ref.Group != v1alpha1.Group || ref.Kind != "HCloudNodeClass" {
					return nil
				}
				// The watch exists for exactly one purpose: releasing the
				// finalizer the moment the last NodeClaim goes away, instead of
				// waiting out the ten-minute requeue. On a class that is not
				// terminating there is nothing to release, and the enqueue would
				// buy a full resolve for nothing. This read is served from the
				// informer cache, so the check is free and the resolve is not.
				key := types.NamespacedName{Name: ref.Name}
				nodeClass := &v1alpha1.HCloudNodeClass{}
				if err := m.GetClient().Get(ctx, key, nodeClass); err != nil || nodeClass.GetDeletionTimestamp().IsZero() {
					return nil
				}
				return []reconcile.Request{{NamespacedName: key}}
			}),
			// Deletes only. Creates and updates are noise: a NodeClaim changing
			// says nothing about whether its class resolves.
			//
			// The cost is that status.placementGroup.serverCount, which grows
			// on NodeClaim CREATE, refreshes only on the five-minute requeue,
			// so the near-capacity warning can arrive up to that late. Accepted
			// deliberately: watching creates would spend seven Hetzner GETs per
			// node launched, on every class, to make one advisory condition
			// timelier, and placement groups are opt-in and unset by default.
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
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
