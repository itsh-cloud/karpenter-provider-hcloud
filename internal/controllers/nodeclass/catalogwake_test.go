package nodeclass

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

// The catalog wake-up is the SOLE source of promptness for a NodeClass blocked
// on the catalog: the requeue in that branch is a deliberately slow backstop,
// because a short one is a full re-resolve throttled by nothing. So every way
// this can silently stop working degrades to a one-minute delay and would never
// be noticed in production, which is exactly why it needs tests here.

func TestPumpForwardsEachSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := make(chan struct{}, 1)
	dst := make(chan event.GenericEvent)
	go pumpRefreshSignal(ctx, src, dst)

	for i := range 3 {
		src <- struct{}{}
		select {
		case <-dst:
		case <-time.After(2 * time.Second):
			t.Fatalf("signal %d was not forwarded", i)
		}
	}
}

// TestPumpStopsOnContextCancel: a leaked pump per Register call would outlive
// its manager and hold the catalog channel forever.
func TestPumpStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := make(chan struct{})
	dst := make(chan event.GenericEvent)

	done := make(chan struct{})
	go func() { pumpRefreshSignal(ctx, src, dst); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return after cancellation")
	}
}

// TestPumpStopsWhileBlockedOnSend covers the other half: the pump parks on the
// send until the manager starts reading, and it must still be releasable.
func TestPumpStopsWhileBlockedOnSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := make(chan struct{}, 1)
	dst := make(chan event.GenericEvent) // never read

	done := make(chan struct{})
	go func() { pumpRefreshSignal(ctx, src, dst); close(done) }()

	src <- struct{}{}
	// Let the pump reach the blocking send before cancelling.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return while blocked on the send")
	}
}

func TestEnqueueCatalogWaiters(t *testing.T) {
	waiting := nodeClassWithValidation("waiting", metav1.ConditionUnknown, reasonCatalogNotFetched)
	empty := nodeClassWithValidation("empty", metav1.ConditionUnknown, reasonCatalogEmpty)
	healthy := nodeClassWithValidation("healthy", metav1.ConditionTrue, "ValidationSucceeded")
	brokenSelector := nodeClassWithValidation("broken", metav1.ConditionFalse, reasonDependenciesNotReady)
	// Never reconciled: no conditions at all.
	fresh := &v1alpha1.HCloudNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "fresh", UID: types.UID("fresh")}}

	c := newFakeClient(waiting, empty, healthy, brokenSelector, fresh)
	got := enqueueCatalogWaiters(c)(context.Background(), nil)

	want := map[string]bool{"waiting": true, "empty": true, "fresh": true}
	if len(got) != len(want) {
		t.Fatalf("enqueued %v, want exactly %v", names(got), want)
	}
	for _, r := range got {
		if !want[r.Name] {
			t.Errorf("enqueued %q, which is not waiting on the catalog", r.Name)
		}
	}
}

// TestEnqueueCatalogWaitersSkipsHealthyClasses states the cost argument as an
// assertion. The catalog refreshes roughly six times an hour; waking a class
// that is already Ready buys nothing, because it re-validates within
// resolvedRequeue anyway, and costs a full pass of Hetzner GETs each time.
func TestEnqueueCatalogWaitersSkipsHealthyClasses(t *testing.T) {
	c := newFakeClient(
		nodeClassWithValidation("a", metav1.ConditionTrue, "ValidationSucceeded"),
		nodeClassWithValidation("b", metav1.ConditionTrue, "LocationsNarrowed"),
	)
	if got := enqueueCatalogWaiters(c)(context.Background(), nil); len(got) != 0 {
		t.Errorf("enqueued %v on a refresh with no class waiting; want none", names(got))
	}
}

// TestEnqueueCatalogWaitersToleratesListFailure: the map func runs on the
// watch path, where returning nothing is correct and panicking is not.
func TestEnqueueCatalogWaitersToleratesListFailure(t *testing.T) {
	if got := enqueueCatalogWaiters(failingReader{})(context.Background(), nil); got != nil {
		t.Errorf("enqueued %v despite the list failing", names(got))
	}
}

var errUnavailable = errors.New("apiserver unavailable")

type failingReader struct{ client.Reader }

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errUnavailable
}

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errUnavailable
}

func nodeClassWithValidation(name string, s metav1.ConditionStatus, reason string) *v1alpha1.HCloudNodeClass {
	nc := &v1alpha1.HCloudNodeClass{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name)}}
	nc.StatusConditions().Set(status.Condition{
		Type:    v1alpha1.ConditionTypeValidationSucceeded,
		Status:  s,
		Reason:  reason,
		Message: reason,
	})
	return nc
}

func names(reqs []reconcile.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Name)
	}
	return out
}
