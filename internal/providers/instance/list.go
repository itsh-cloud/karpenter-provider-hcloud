package instance

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

// listTimeout bounds one shared listing. Neither hcloud-go's HTTP client nor
// core's reconcilers set a deadline, so without it one hung request would
// hold drift, termination and garbage collection behind it. Sixty seconds
// clears the roughly 31s hcloud-go can spend backing off between retries.
const listTimeout = 60 * time.Second

// listCache serves one recent server listing to every caller.
type listCache struct {
	servers hcloudapi.Servers
	ttl     time.Duration
	timeout time.Duration
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
	return &listCache{servers: servers, ttl: ttl, timeout: listTimeout, now: now}
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

	ch := c.group.DoChan(selector, func() (_ any, err error) {
		// DoChan re-panics on a goroutine of its own, beyond the reach of
		// controller-runtime's reconcile recovery, so a panic here would kill
		// the process. Every waiter gets it as an error instead.
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("listing servers panicked: %v", r)
				log.FromContext(ctx).Error(err, "recovered a panic in the shared server listing", "stack", string(debug.Stack()))
			}
		}()

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

		// Detached from whichever caller started the flight, whose
		// cancellation would otherwise fail every caller sharing it.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		servers, err := c.servers.List(flightCtx, selector)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.cached, c.fetchedAt = servers, c.now()
		c.mu.Unlock()
		return servers, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.([]*hcloudapi.Server), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
// Served from the list cache, refreshed through List's single flight when
// stale, so a drift pass over N NodeClaims costs one listing rather than N
// point reads. Drift calls this once per NodeClaim per reconcile and its
// controller watches Pods unfiltered, so on a busy cluster it runs well above
// the 5-minute floor, against a Hetzner budget of 3600 requests/hour shared by
// every client in the project.
//
// A miss is not an answer, and neither is a failed listing. Absence from the
// listing only means the listing predates the server, but nil here is read as
// "gone, or not ours", and core's termination controller drops the Node's
// finalizer on it, skipping the drain. So a miss falls through to a point read,
// and a listing error is returned as an error rather than retried per id, which
// under a rate limit would multiply the calls that caused it.
//
// A hit is the same struct a point read builds (both map through
// serverFromHcloud) and carries every field serverDrift reads, so it is older
// rather than weaker. Within the TTL a server changed INTO conformance can
// still read as drifted, which costs a replacement. It points into the cached
// slice and is shared. Do not mutate it.
func (p *Provider) Get(ctx context.Context, providerID string) (*hcloudapi.Server, error) {
	if p.clusterName == "" {
		return nil, errNoClusterName
	}
	id, err := hcloudapi.ServerIDFromProviderID(providerID)
	if err != nil {
		return nil, err
	}
	servers, err := p.cache.list(ctx, hcloudapi.ManagedBySelector(p.clusterName))
	if err != nil {
		return nil, err
	}
	var srv *hcloudapi.Server
	for _, s := range servers {
		if s != nil && s.ID == id {
			srv = s
			break
		}
	}
	if srv == nil {
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
