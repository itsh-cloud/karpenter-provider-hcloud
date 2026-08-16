// Package catalog keeps a periodically refreshed snapshot of the Hetzner
// server-type catalog.
package catalog

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

const (
	// DefaultRefreshInterval is how often the catalog is refetched.
	//
	// Ten minutes rather than the once-a-minute an earlier draft assumed,
	// because the availability flag is not consulted (see the instancetype
	// package) and what remains is slow-moving: server types, their locations
	// and their prices. Hetzner's rate limit is 3600 requests/hour PER
	// PROJECT and is shared with the CCM and the CSI driver, so this is
	// budget someone else also needs. Upstream cluster-autoscaler uses the
	// same ten minutes for server types.
	DefaultRefreshInterval = 10 * time.Minute

	// DefaultJitter is added to each refresh as a random 0..jitter.
	//
	// The controller runs two replicas for leader election, and a fleet-wide
	// restart would otherwise synchronise their refreshes forever. Same
	// reasoning as cluster-autoscaler's 5-60s cache jitter.
	DefaultJitter = 60 * time.Second
)

// Snapshot is an immutable view of the catalog at one instant.
type Snapshot struct {
	// Generation increments on every successful refresh, so readers can
	// cheaply tell whether anything derived from this snapshot is stale.
	Generation uint64

	ServerTypes        []hcloudapi.ServerType
	PrimaryIPv4Monthly float64

	// FetchedAt is when this snapshot was retrieved, not when it was served.
	FetchedAt time.Time
}

// Provider serves the most recent good snapshot.
type Provider struct {
	client hcloudapi.Catalog

	interval time.Duration
	jitter   time.Duration
	now      func() time.Time
	rand     *rand.Rand
	randMu   sync.Mutex

	generation atomic.Uint64
	snapshot   atomic.Pointer[Snapshot]

	// stale reports whether the last refresh attempt failed. The snapshot is
	// still served in that case: an API blip must never make Karpenter believe
	// the cluster has zero instance types, which would look exactly like
	// everything being unschedulable.
	stale atomic.Bool
}

// Option configures a Provider.
type Option func(*Provider)

// WithInterval overrides the refresh interval.
func WithInterval(d time.Duration) Option { return func(p *Provider) { p.interval = d } }

// WithJitter overrides the refresh jitter.
func WithJitter(d time.Duration) Option { return func(p *Provider) { p.jitter = d } }

// WithClock overrides the clock, for tests.
func WithClock(now func() time.Time) Option { return func(p *Provider) { p.now = now } }

// NewProvider returns a catalog provider. Call Refresh once before serving, or
// Start to keep it fresh.
func NewProvider(client hcloudapi.Catalog, opts ...Option) *Provider {
	p := &Provider{
		client:   client,
		interval: DefaultRefreshInterval,
		jitter:   DefaultJitter,
		now:      time.Now,
		rand:     rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // jitter, not crypto
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Get returns the most recent snapshot, or nil if none has been fetched yet.
//
// A nil return means "cannot evaluate", not "no capacity exists". Callers must
// surface it as such: telling Karpenter there are zero instance types would
// make every pending pod look permanently unschedulable.
func (p *Provider) Get() *Snapshot { return p.snapshot.Load() }

// Stale reports whether the last refresh failed. The snapshot is still served.
func (p *Provider) Stale() bool { return p.stale.Load() }

// Refresh fetches the catalog once.
//
// On failure the previous snapshot is retained and Stale() becomes true. Only
// a total absence of any prior snapshot leaves Get() returning nil.
func (p *Provider) Refresh(ctx context.Context) error {
	serverTypes, err := p.client.ServerTypes(ctx)
	if err != nil {
		p.stale.Store(true)
		return fmt.Errorf("refreshing server types: %w", err)
	}

	// A missing IPv4 surcharge is not fatal: it shifts every offering that has
	// a public IP by the same amount, so relative ordering is unaffected.
	primaryIPv4, err := p.client.PrimaryIPv4MonthlyNet(ctx)
	if err != nil {
		primaryIPv4 = 0
	}

	p.snapshot.Store(&Snapshot{
		Generation:         p.generation.Add(1),
		ServerTypes:        serverTypes,
		PrimaryIPv4Monthly: primaryIPv4,
		FetchedAt:          p.now(),
	})
	p.stale.Store(false)
	return nil
}

// Start refreshes until ctx is cancelled. It performs one refresh immediately
// so that a caller can fail fast on a bad token rather than discovering it at
// the first scale-up.
func (p *Provider) Start(ctx context.Context) error {
	if err := p.Refresh(ctx); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.nextInterval()):
			// A failure here is deliberately not returned: the loop keeps
			// running and keeps serving the last good snapshot.
			_ = p.Refresh(ctx)
		}
	}
}

func (p *Provider) nextInterval() time.Duration {
	if p.jitter <= 0 {
		return p.interval
	}
	p.randMu.Lock()
	defer p.randMu.Unlock()
	return p.interval + time.Duration(p.rand.Int63n(int64(p.jitter)))
}
