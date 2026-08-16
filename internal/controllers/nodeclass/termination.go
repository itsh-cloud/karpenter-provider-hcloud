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
// NodeClaims re-checks and re-publishes its event. The wait is indefinite by
// design, so the interval only controls how often the explanation is refreshed.
const terminationBlockedRequeue = 10 * time.Minute

// Termination releases an HCloudNodeClass once nothing references it.
//
// This entire path is provider-owned. Karpenter core declares no NodeClass
// finalizer and ships no NodeClass termination controller, and it does not
// cascade: deleting a NodeClass while NodeClaims reference it is, upstream, a
// no-op that leaves those NodeClaims pointing at nothing. The only visible
// effect is the NodePool going NodeClassReady=False/NodeClassTerminating, which
// stops NEW provisioning while every existing node keeps running, keeps being
// drift-evaluated and keeps being consolidated against a class that is gone.
//
// There is nothing provider-side to clean up here. This provider creates no
// per-class Hetzner resource: networks, firewalls, ssh keys and placement
// groups are all referenced, never owned. The per-NodeClaim bootstrap token
// Secrets carry an ownerReference to their NodeClaim and are collected with it.
// So the finalizer's whole job is the ordering gate.
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

	// A field-index lookup on the three spec.nodeClassRef indexes. An
	// unregistered index ERRORS rather than returning an empty list, in both
	// the real cache and the fake client, so a missing index cannot make this
	// release the finalizer while NodeClaims still exist.
	//
	// The hazard is the opposite one. Core's setupIndexers fails OPEN when the
	// NodeClaim CRD is absent at operator start: it logs and continues, leaving
	// the index unregistered. Indexes are only registered at manager
	// construction, so installing the CRD afterwards does not fix it, and this
	// List then errors on every pass forever. A deleted HCloudNodeClass hangs
	// on its finalizer until the operator is restarted or the finalizer is
	// patched off by hand.
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
		// Blocks indefinitely, on purpose. There is no safe timeout: releasing
		// the class while nodes still reference it does not delete those nodes,
		// it only removes the record of how they were built.
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
