package instancetype

import (
	"sync"
	"testing"
	"time"
)

func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestMarkAndExpiry(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	u := NewUnavailableWithOptions(5*time.Minute, fixedClock(&now))

	if u.Has("cx43", "nbg1") {
		t.Fatal("empty cache reported a suppression")
	}

	u.Mark("cx43", "nbg1", "resource_unavailable")

	if !u.Has("cx43", "nbg1") {
		t.Error("marked pair not suppressed")
	}
	// Suppression is per (type, location). A stockout in one location says
	// nothing about another, and over-suppressing would strand capacity.
	if u.Has("cx43", "hel1") {
		t.Error("suppression leaked across locations")
	}
	if u.Has("cx33", "nbg1") {
		t.Error("suppression leaked across server types")
	}

	if code, ok := u.Code("cx43", "nbg1"); !ok || code != "resource_unavailable" {
		t.Errorf("Code() = %q, %v; want the code that caused suppression", code, ok)
	}

	// Just before expiry it is still suppressed; just after, it is not. The
	// second half matters most: this is how the provider notices stock has
	// returned, which is the entire point of the project.
	now = now.Add(5*time.Minute - time.Nanosecond)
	if !u.Has("cx43", "nbg1") {
		t.Error("expired early")
	}
	now = now.Add(2 * time.Nanosecond)
	if u.Has("cx43", "nbg1") {
		t.Error("did not expire; a permanent suppression would keep an expensive " +
			"fallback node alive forever after stock returned")
	}
	if _, ok := u.Code("cx43", "nbg1"); ok {
		t.Error("Code() still reports an expired suppression")
	}
}

func TestMarkRefreshesTTL(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	u := NewUnavailableWithOptions(5*time.Minute, fixedClock(&now))

	u.Mark("cx43", "nbg1", "resource_unavailable")
	now = now.Add(4 * time.Minute)
	u.Mark("cx43", "nbg1", "placement_error")

	now = now.Add(2 * time.Minute) // past the original expiry, inside the new one
	if !u.Has("cx43", "nbg1") {
		t.Error("re-marking did not extend the suppression")
	}
	if code, _ := u.Code("cx43", "nbg1"); code != "placement_error" {
		t.Errorf("Code() = %q, want the most recent cause", code)
	}
}

func TestMarkLocation(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	u := NewUnavailableWithOptions(5*time.Minute, fixedClock(&now))

	u.Mark("cx43", "nbg1", "resource_unavailable")
	u.Mark("cx33", "nbg1", "resource_unavailable")
	u.Mark("cx43", "hel1", "resource_unavailable")

	now = now.Add(4 * time.Minute)
	u.MarkLocation("nbg1", "maintenance")

	now = now.Add(2 * time.Minute) // past the original TTL for all three
	if !u.Has("cx43", "nbg1") || !u.Has("cx33", "nbg1") {
		t.Error("location-wide mark did not refresh every type in it")
	}
	if u.Has("cx43", "hel1") {
		t.Error("location-wide mark leaked into another location")
	}
}

func TestLenExpiresEntries(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	u := NewUnavailableWithOptions(5*time.Minute, fixedClock(&now))

	u.Mark("cx43", "nbg1", "resource_unavailable")
	u.Mark("cx53", "fsn1", "placement_error")
	if got := u.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}

	now = now.Add(6 * time.Minute)
	// Len also reaps, so a long-lived controller does not accumulate an entry
	// per (type, location) pair it has ever failed on.
	if got := u.Len(); got != 0 {
		t.Errorf("Len() = %d after expiry, want 0", got)
	}
}

func TestConcurrentAccess(t *testing.T) {
	u := NewUnavailable()
	var wg sync.WaitGroup

	// Karpenter provisions the whole pending set at once, so several
	// goroutines mark and read this concurrently.
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); u.Mark("cx43", "nbg1", "resource_unavailable") }()
		go func() { defer wg.Done(); _ = u.Has("cx43", "nbg1") }()
	}
	wg.Wait()

	if !u.Has("cx43", "nbg1") {
		t.Error("lost a mark under concurrency")
	}
}

// TestSnapshotReapsExpiredEntries.
//
// Snapshot is what the metric sync rebuilds from, so an expired entry lingering
// here becomes a gauge that reads as a permanent stockout long after stock
// returned. That is the exact question this project exists to answer correctly,
// so it is the reaping, not the reporting, that matters.
func TestSnapshotReapsExpiredEntries(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	u := NewUnavailableWithOptions(5*time.Minute, fixedClock(&now))

	u.Mark("cx33", "nbg1", "resource_unavailable")
	u.Mark("cx43", "fsn1", "placement_error")

	got := u.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot() = %+v, want both suppressions", got)
	}
	for _, s := range got {
		if s.ServerType == "" || s.Location == "" || s.Code == "" {
			t.Errorf("Snapshot() entry %+v is missing a label the gauge needs", s)
		}
	}

	now = now.Add(6 * time.Minute)
	if got := u.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot() = %+v after expiry; a stale entry reads as a stockout that has already ended", got)
	}
}

// TestLenAgreesWithSnapshot: both answer "what is suppressed right now", so a
// divergence would mean two definitions of the same thing.
func TestLenAgreesWithSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	u := NewUnavailableWithOptions(5*time.Minute, fixedClock(&now))

	u.Mark("cx33", "nbg1", "resource_unavailable")
	u.Mark("cx43", "nbg1", "resource_unavailable")
	if u.Len() != len(u.Snapshot()) {
		t.Errorf("Len() = %d but Snapshot() has %d entries", u.Len(), len(u.Snapshot()))
	}

	now = now.Add(6 * time.Minute)
	if u.Len() != 0 || len(u.Snapshot()) != 0 {
		t.Errorf("after expiry Len() = %d, Snapshot() has %d", u.Len(), len(u.Snapshot()))
	}
}
