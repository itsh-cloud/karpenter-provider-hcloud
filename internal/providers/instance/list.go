package instance

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// DefaultListTTL is how long a server listing is reused.
//
// Karpenter core calls List from several controllers on their own schedules:
// garbage collection, the NodeClaim informer, and drift evaluation. Uncached,
// a quiet cluster spends a meaningful share of Hetzner's 3600 requests/hour,
// which is a per-project budget shared with hcloud-CCM and the CSI driver, on
// re-reading a list that changes only when a node is created or removed.
//
// Thirty seconds is well inside karpenter's own reconcile intervals, so no
// caller waits on stale data long enough to act wrongly: the worst case is one
// extra pass before an orphan is noticed.
const DefaultListTTL = 30 * time.Second

// listCache serves one recent server listing to every caller.
type listCache struct {
	servers hcloudapi.Servers
	ttl     time.Duration
	now     func() time.Time

	// group collapses concurrent misses into ONE upstream call. Without it,
	// three controllers waking together on a cold cache issue three identical
	// listings, which is precisely the burst the cache exists to prevent.
	group singleflight.Group

	mu        sync.RWMutex
	cached    []*hcloudapi.Server
	fetchedAt time.Time
}

func newListCache(servers hcloudapi.Servers, ttl time.Duration, now func() time.Time) *listCache {
	if ttl <= 0 {
		ttl = DefaultListTTL
	}
	if now == nil {
		now = time.Now
	}
	return &listCache{servers: servers, ttl: ttl, now: now}
}

// list returns the servers matching selector, from cache when fresh.
func (c *listCache) list(ctx context.Context, selector string) ([]*hcloudapi.Server, error) {
	c.mu.RLock()
	if c.cached != nil && c.now().Sub(c.fetchedAt) < c.ttl {
		out := c.cached
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	v, err, _ := c.group.Do(selector, func() (any, error) {
		// Re-checked inside the flight: the goroutine that lost the race to
		// start it would otherwise refetch immediately after the winner
		// finished.
		c.mu.RLock()
		fresh := c.cached != nil && c.now().Sub(c.fetchedAt) < c.ttl
		cached := c.cached
		c.mu.RUnlock()
		if fresh {
			return cached, nil
		}

		servers, err := c.servers.List(ctx, selector)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.cached, c.fetchedAt = servers, c.now()
		c.mu.Unlock()
		return servers, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]*hcloudapi.Server), nil
}

// get answers from the cached listing, and reports whether it could answer.
//
// A miss is not an answer: the caller must fall through to a point read.
// Absence from the listing only means the listing predates the server, but
// Provider.Get's nil is read as "gone, or not ours", and core's termination
// controller drops the Node's finalizer on it, skipping the drain. So only a
// positive hit is served; a stale cache and an absent id both return false.
//
// The Server points into the cached slice and is shared. Do not mutate it.
func (c *listCache) get(id int64) (*hcloudapi.Server, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil || c.now().Sub(c.fetchedAt) >= c.ttl {
		return nil, false
	}
	for _, srv := range c.cached {
		if srv != nil && srv.ID == id {
			return srv, true
		}
	}
	return nil, false
}

// invalidate drops the cached listing.
//
// Called after this provider creates or deletes a server, so the very next
// List reflects a change we already know about rather than waiting out the TTL.
// Without it, core can garbage-collect a server it created seconds ago because
// the cached listing predates it.
func (c *listCache) invalidate() {
	c.mu.Lock()
	c.cached, c.fetchedAt = nil, time.Time{}
	c.mu.Unlock()
}

// List returns every server this cluster's provider owns.
func (p *Provider) List(ctx context.Context) ([]*hcloudapi.Server, error) {
	if p.clusterName == "" {
		// An empty selector would match the whole project, including the
		// control plane. Every caller of this feeds deletion decisions.
		return nil, errNoClusterName
	}
	servers, err := p.cache.list(ctx, hcloudapi.ManagedBySelector(p.clusterName))
	if err != nil {
		return nil, err
	}
	// Re-checked in process even though the selector already filtered
	// server-side, so all three read paths apply the same rule rather than one
	// of them trusting a query string. This is what core's garbage collector
	// compares its NodeClaims against, so anything wrongly present here is
	// something core may adopt and later delete.
	owned := make([]*hcloudapi.Server, 0, len(servers))
	for _, srv := range servers {
		if srv.IsManagedBy(p.clusterName) {
			owned = append(owned, srv)
		}
	}
	return owned, nil
}

// Get returns one server by provider id, or nil if it is gone.
//
// Served from the list cache when it holds the id. Drift calls this once per
// NodeClaim per reconcile and its controller watches Pods unfiltered, so on a
// busy cluster it runs well above the 5-minute floor, against a Hetzner budget
// of 3600 requests/hour shared by every client in the project. A rate-limited
// call is worse than it looks: hcloud-go retries it five times by default.
//
// A hit is the same struct a point read builds (both map through
// serverFromHcloud) and carries every field serverDrift reads, so it is older
// rather than weaker. Within the TTL a server changed INTO conformance can
// still read as drifted, which costs a replacement.
func (p *Provider) Get(ctx context.Context, providerID string) (*hcloudapi.Server, error) {
	id, err := hcloudapi.ServerIDFromProviderID(providerID)
	if err != nil {
		return nil, err
	}
	srv, ok := p.cache.get(id)
	if !ok {
		if srv, err = p.servers.Get(ctx, id); err != nil {
			return nil, err
		}
	}
	if srv == nil {
		return nil, nil
	}
	// Ownership is checked on read, not only on delete. A server this cluster
	// does not own must be invisible to karpenter, because everything
	// downstream of Get treats what it returns as a node it may manage.
	if !srv.IsManagedBy(p.clusterName) {
		return nil, nil
	}
	return srv, nil
}

// Delete removes the server behind a provider id.
//
// The ownership check is the single most important line in this provider: the
// blast radius of getting it wrong is deleting the control plane. Servers this
// provider did not create carry no karpenter label, which is what makes the
// check meaningful. It fails closed, so no labels, no label key, or an empty
// cluster name all mean "not ours".
//
// The read below deliberately skips the list cache, unlike Get: this is the
// ownership gate in front of a destructive call, so it reads live state.
func (p *Provider) Delete(ctx context.Context, providerID string) error {
	id, err := hcloudapi.ServerIDFromProviderID(providerID)
	if err != nil {
		return err
	}
	srv, err := p.servers.Get(ctx, id)
	if err != nil {
		return err
	}
	if srv == nil {
		return &hcloudapi.NotFoundError{Kind: "server", Selector: providerID}
	}
	if !srv.IsManagedBy(p.clusterName) {
		return errNotManaged{name: srv.Name, id: srv.ID}
	}

	if err := p.servers.Delete(ctx, id); err != nil {
		return err
	}
	p.cache.invalidate()
	return nil
}
