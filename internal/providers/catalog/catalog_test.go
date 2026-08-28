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

// TestFailureKeepsLastGoodSnapshot: a Hetzner API blip must never make
// Karpenter believe the cluster has zero instance types, which would present as
// every pending pod being permanently unschedulable with no capacity anywhere.
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
// startup error, not something discovered at the first scale-up. It will not
// fix itself, and every retry burns rate limit shared with the CCM and CSI.
func TestStartFailsFastOnRejectedCredential(t *testing.T) {
	f := &fakeCatalog{err: hcloud.Error{Code: hcloud.ErrorCodeUnauthorized, Message: "unable to authenticate"}}
	p := NewProvider(f)

	if err := p.Start(context.Background()); err == nil {
		t.Fatal("Start should return a rejected credential")
	}
}

// TestStartSurvivesTransientFirstRefresh: Start runs as a manager Runnable, so
// returning an error restarts the pod. Treating a Hetzner outage or a 429 as
// fatal would CrashLoopBackOff for its duration, take down the controllers
// needing no Hetzner access, and re-issue the rate-limited request on each
// restart.
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

// TestRefreshSignalsWaiters: the wake-up is the only thing that makes a
// NodeClass blocked on the catalog recover promptly, since the requeue on that
// branch is a deliberately slow backstop (a short one is a full re-resolve that
// no rate limiter throttles). A broken signal degrades silently to a minute of
// delay rather than failing.
func TestRefreshSignalsWaiters(t *testing.T) {
	f := &fakeCatalog{types: testTypes()}
	p := NewProvider(f)

	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	select {
	case <-p.Refreshed():
	default:
		t.Fatal("a successful refresh signalled no waiter")
	}
}

// TestFailedRefreshDoesNotSignal: the signal means "there is a new snapshot",
// so signalling on failure wakes every waiting NodeClass to find the same
// nothing, which is the poll this design exists to remove.
func TestFailedRefreshDoesNotSignal(t *testing.T) {
	f := &fakeCatalog{err: errors.New("dial tcp: i/o timeout")}
	p := NewProvider(f)

	if err := p.Refresh(context.Background()); err == nil {
		t.Fatal("expected the refresh to fail")
	}
	select {
	case <-p.Refreshed():
		t.Error("a failed refresh signalled a waiter")
	default:
	}
}

// TestRefreshNeverBlocksOnAnAbsentListener keeps the refresh loop independent
// of its consumers. With a blocking send, a controller not yet reading (the
// normal state before the manager starts) would stall the catalog and every
// NodeClass waiting on it.
func TestRefreshNeverBlocksOnAnAbsentListener(t *testing.T) {
	f := &fakeCatalog{types: testTypes()}
	p := NewProvider(f)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 {
			if err := p.Refresh(context.Background()); err != nil {
				t.Errorf("Refresh: %v", err)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Refresh blocked with nobody reading the wake-up channel")
	}

	// Coalesced to one: the signal carries no payload, so a pending wake-up
	// already covers every refresh that happened since.
	n := 0
	for {
		select {
		case <-p.Refreshed():
			n++
			continue
		default:
		}
		break
	}
	if n != 1 {
		t.Errorf("five refreshes left %d pending wake-ups, want exactly 1", n)
	}
}

// TestStaleForReportsNeverFetched: subtracting from the zero time prints a
// meaningless 2562047h, and this is the common case, since the first refresh
// failing is the precondition for the log line to exist at all.
func TestStaleForReportsNeverFetched(t *testing.T) {
	p := NewProvider(&fakeCatalog{err: errors.New("boom")})
	if got := p.staleFor(); got != "never fetched" {
		t.Errorf("staleFor() = %q before any successful fetch, want %q", got, "never fetched")
	}

	f := &fakeCatalog{types: testTypes()}
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	p2 := NewProvider(f, WithClock(func() time.Time { return now }))
	if err := p2.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(90 * time.Second)
	if got := p2.staleFor(); got != "1m30s" {
		t.Errorf("staleFor() = %q, want 1m30s", got)
	}
}
