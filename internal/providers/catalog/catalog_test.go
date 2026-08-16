package catalog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

type fakeCatalog struct {
	mu    sync.Mutex
	types []hcloudapi.ServerType
	ipv4  float64
	err   error
	calls int
}

func (f *fakeCatalog) ServerTypes(context.Context) ([]hcloudapi.ServerType, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.types, nil
}

func (f *fakeCatalog) PrimaryIPv4MonthlyNet(context.Context) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ipv4, f.err
}

func (f *fakeCatalog) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func testTypes() []hcloudapi.ServerType {
	return []hcloudapi.ServerType{{ID: 116, Name: "cx43", Cores: 8, MemoryGiB: 16, DiskGB: 160}}
}

func TestRefreshPopulatesSnapshot(t *testing.T) {
	f := &fakeCatalog{types: testTypes(), ipv4: 0.60}
	p := NewProvider(f)

	if p.Get() != nil {
		t.Error("snapshot present before any refresh")
	}

	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	s := p.Get()
	if s == nil {
		t.Fatal("no snapshot after refresh")
	}
	if len(s.ServerTypes) != 1 || s.ServerTypes[0].Name != "cx43" {
		t.Errorf("unexpected server types: %+v", s.ServerTypes)
	}
	if s.PrimaryIPv4Monthly != 0.60 {
		t.Errorf("ipv4 surcharge = %v, want 0.60", s.PrimaryIPv4Monthly)
	}
	if s.Generation != 1 {
		t.Errorf("generation = %d, want 1", s.Generation)
	}
	if p.Stale() {
		t.Error("marked stale after a successful refresh")
	}
}

// TestFailureKeepsLastGoodSnapshot is the property that matters most here.
//
// A Hetzner API blip must never make Karpenter believe the cluster has zero
// instance types. That would present as every pending pod being permanently
// unschedulable with no capacity anywhere, which is both alarming and wrong.
func TestFailureKeepsLastGoodSnapshot(t *testing.T) {
	f := &fakeCatalog{types: testTypes()}
	p := NewProvider(f)

	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}
	first := p.Get()

	f.setErr(errors.New("hcloud is having a day"))
	if err := p.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error from a failing refresh")
	}

	got := p.Get()
	if got == nil {
		t.Fatal("snapshot dropped on refresh failure; Karpenter would see zero instance types")
	}
	if got.Generation != first.Generation {
		t.Errorf("generation advanced on a failed refresh: %d", got.Generation)
	}
	if !p.Stale() {
		t.Error("Stale() should report the failure so it can be alerted on")
	}

	// And it recovers.
	f.setErr(nil)
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("recovery Refresh: %v", err)
	}
	if p.Stale() {
		t.Error("still stale after a successful refresh")
	}
	if p.Get().Generation != first.Generation+1 {
		t.Error("generation did not advance after recovery")
	}
}

// TestIPv4PriceFailureIsNotFatal: the surcharge shifts every offering that has
// a public IP by the same amount, so losing it does not change which offering
// is cheapest. Failing the whole refresh over it would be worse.
func TestIPv4PriceFailureIsNotFatal(t *testing.T) {
	f := &fakeCatalog{types: testTypes()}
	p := NewProvider(f)

	// ServerTypes succeeds, the pricing call does not.
	f.ipv4 = 0
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if p.Get() == nil {
		t.Fatal("no snapshot")
	}
}

func TestGenerationIncrementsPerRefresh(t *testing.T) {
	f := &fakeCatalog{types: testTypes()}
	p := NewProvider(f)

	for i := 1; i <= 3; i++ {
		if err := p.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh %d: %v", i, err)
		}
		if got := p.Get().Generation; got != uint64(i) {
			t.Errorf("generation = %d, want %d", got, i)
		}
	}
}

// TestJitterStaysInRange: the jitter exists so two replicas do not synchronise
// their refreshes forever after a fleet restart.
func TestJitterStaysInRange(t *testing.T) {
	p := NewProvider(&fakeCatalog{}, WithInterval(10*time.Minute), WithJitter(time.Minute))

	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := p.nextInterval()
		if d < 10*time.Minute || d >= 11*time.Minute {
			t.Fatalf("interval %v outside [10m, 11m)", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Error("jitter produced a constant interval; replicas would stay synchronised")
	}
}

func TestZeroJitterIsExact(t *testing.T) {
	p := NewProvider(&fakeCatalog{}, WithInterval(time.Minute), WithJitter(0))
	if got := p.nextInterval(); got != time.Minute {
		t.Errorf("nextInterval = %v, want exactly 1m", got)
	}
}

// TestStartFailsFastOnRejectedCredential: a bad token should be an immediate
// startup error, not something discovered at the first scale-up during an
// incident. It will not fix itself, and every retry burns rate limit shared
// with the CCM and the CSI driver.
func TestStartFailsFastOnRejectedCredential(t *testing.T) {
	f := &fakeCatalog{err: hcloud.Error{Code: hcloud.ErrorCodeUnauthorized, Message: "unable to authenticate"}}
	p := NewProvider(f)

	if err := p.Start(context.Background()); err == nil {
		t.Fatal("Start should return a rejected credential")
	}
}

// TestStartSurvivesTransientFirstRefresh.
//
// Start runs as a manager Runnable, so returning an error fails the manager and
// restarts the pod. Treating a Hetzner outage or a 429 as fatal therefore
// CrashLoopBackOffs for the duration of the outage, taking down the controllers
// that need no Hetzner access at all, and the restarts themselves re-issue the
// request that is being rate limited.
func TestStartSurvivesTransientFirstRefresh(t *testing.T) {
	f := &fakeCatalog{err: errors.New("dial tcp: i/o timeout")}
	p := NewProvider(f, WithInterval(10*time.Millisecond), WithJitter(0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	select {
	case err := <-done:
		cancel()
		t.Fatalf("Start returned %v on a transient failure; the pod would CrashLoopBackOff through the outage", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after cancellation")
	}

	// Still no snapshot, and Get must report that as nil rather than as an
	// empty catalog: zero instance types looks exactly like every pod being
	// permanently unschedulable.
	if p.Get() != nil {
		t.Error("Get returned a snapshot after only failed refreshes")
	}
	if !p.Stale() {
		t.Error("Stale is false after a failed refresh")
	}
}

func TestStartStopsOnContextCancel(t *testing.T) {
	f := &fakeCatalog{types: testTypes()}
	p := NewProvider(f, WithInterval(10*time.Millisecond), WithJitter(0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after cancellation")
	}

	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls < 2 {
		t.Errorf("only %d refreshes; the loop does not appear to have ticked", calls)
	}
}
