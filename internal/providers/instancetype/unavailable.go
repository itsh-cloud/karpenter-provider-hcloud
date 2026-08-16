// Package instancetype builds the Karpenter instance-type catalog from the
// Hetzner Cloud API.
package instancetype

import (
	"sync"
	"time"
)

// DefaultUnavailableTTL is how long an observed capacity failure suppresses a
// (server type, location) pair.
//
// Longer than the three minutes some providers use, because Hetzner stockouts
// last days rather than seconds, so a short TTL just spends one failed create
// and one rate-limit slot per TTL to learn what we already knew. Not much
// longer, because the second half of the requirement is noticing promptly when
// stock returns: that is the whole point of the project.
const DefaultUnavailableTTL = 5 * time.Minute

// Unavailable records (server type, location) pairs that recently failed to
// provision, so the next scheduling pass skips them instead of re-attempting
// the same doomed combination.
//
// # Why this is the primary availability signal
//
// Hetzner publishes an availability flag per (server type, location), in
// Location.ServerTypes.Available and ServerType.Locations[].Available. It is
// tempting to gate provisioning on it. Do not. Measured against the live API,
// that flag is neither sufficient nor necessary:
//
//   - Not sufficient. A type reported available can still fail to place. This
//     is well attested operationally: repeated resource_unavailable responses
//     for a type that the API continued to list as available.
//
//   - Not necessary. On 2026-08-16, cx43 and cx53 were both reported
//     available:false for nbg1, in the locations listing and in the create
//     response's own embedded server_type.locations. Ordering them in nbg1
//     succeeded anyway: HTTP 201, create_server action reaching success, a
//     running server. Reproduced three times across the two types.
//
// So the flag does not gate ordering, and treating it as a hard filter would
// be actively harmful here: it would exclude exactly the cheap types we most
// want to return to, keeping an expensive fallback node alive indefinitely
// while the API quietly would have sold us the cheaper one. That is the
// failure this provider exists to prevent, reintroduced by its own fix.
//
// The only trustworthy signal is a real failure, which is what this records.
// The published flag is still worth exporting as a metric, because divergence
// between "what Hetzner says" and "what Hetzner does" is interesting, but it
// must not decide anything.
type Unavailable struct {
	mu    sync.RWMutex
	ttl   time.Duration
	now   func() time.Time
	items map[key]entry
}

type key struct {
	serverType string
	location   string
}

type entry struct {
	until time.Time
	code  string
}

// NewUnavailable returns a cache using the default TTL and the real clock.
func NewUnavailable() *Unavailable {
	return &Unavailable{ttl: DefaultUnavailableTTL, now: time.Now, items: map[key]entry{}}
}

// NewUnavailableWithOptions returns a cache with an explicit TTL and clock,
// for tests.
func NewUnavailableWithOptions(ttl time.Duration, now func() time.Time) *Unavailable {
	if now == nil {
		now = time.Now
	}
	return &Unavailable{ttl: ttl, now: now, items: map[key]entry{}}
}

// Mark suppresses one (server type, location) pair for the TTL.
//
// Call this only for genuine capacity failures. In particular never call it
// for a rate-limit error: that is caused by our own request volume, and
// suppressing offerings because of it converts a slowdown into a
// self-inflicted capacity outage.
func (u *Unavailable) Mark(serverType, location, code string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.items[key{serverType, location}] = entry{until: u.now().Add(u.ttl), code: code}
}

// MarkLocation suppresses every recorded type in a location, and is the
// right response to a location-wide signal such as maintenance.
//
// It can only suppress pairs already known to this cache, so callers with the
// full catalog should iterate it and call Mark per type instead.
func (u *Unavailable) MarkLocation(location, code string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	until := u.now().Add(u.ttl)
	for k := range u.items {
		if k.location == location {
			u.items[k] = entry{until: until, code: code}
		}
	}
}

// Has reports whether the pair is currently suppressed.
func (u *Unavailable) Has(serverType, location string) bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	e, ok := u.items[key{serverType, location}]
	return ok && u.now().Before(e.until)
}

// Code returns the error code that caused the current suppression, for
// metrics and events.
func (u *Unavailable) Code(serverType, location string) (string, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	e, ok := u.items[key{serverType, location}]
	if !ok || !u.now().Before(e.until) {
		return "", false
	}
	return e.code, true
}

// Len reports how many pairs are currently suppressed, expiring stale entries
// as a side effect so the map does not grow without bound.
func (u *Unavailable) Len() int {
	return len(u.Snapshot())
}

// Suppression is one currently suppressed pair.
type Suppression struct {
	ServerType string
	Location   string
	Code       string
}

// Snapshot returns the pairs suppressed right now, reaping expired entries as
// it goes.
//
// Exists so a metric can be SYNCED rather than only incremented. A gauge that
// is set on failure and never cleared reads as a permanent stockout long after
// stock returned, which is precisely the wrong thing to show an operator
// deciding whether capacity has recovered.
func (u *Unavailable) Snapshot() []Suppression {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	out := make([]Suppression, 0, len(u.items))
	for k, e := range u.items {
		if !now.Before(e.until) {
			delete(u.items, k)
			continue
		}
		out = append(out, Suppression{ServerType: k.serverType, Location: k.location, Code: e.code})
	}
	return out
}
