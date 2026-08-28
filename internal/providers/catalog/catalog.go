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

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/metrics"
)

const (
	// DefaultRefreshInterval is how often the catalog is refetched.
	//
	// What this fetches is slow-moving (server types, locations, prices) and
	// Hetzner's 3600 requests/hour limit is PER PROJECT, shared with the CCM
	// and the CSI driver, so this is budget someone else also needs.
	DefaultRefreshInterval = 10 * time.Minute

	// DefaultJitter is added to each refresh as a random 0..jitter, so that a
	// fleet-wide restart does not synchronise replicas' refreshes forever.
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
	// still served in that case, see Get.
	stale atomic.Bool

	// refreshed wakes controllers waiting on the catalog instead of making them
	// poll. Buffered by one and written non-blocking: the signal means "look
	// again", not "here is what changed", so coalescing refreshes is correct
	// and a reader that is not listening must never stall the refresh loop.
	refreshed chan struct{}
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
		client:    client,
		interval:  DefaultRefreshInterval,
		jitter:    DefaultJitter,
		now:       time.Now,
		rand:      rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // jitter, not crypto
		refreshed: make(chan struct{}, 1),
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

// Refreshed yields a value after each successful refresh.
//
// A controller that cannot proceed without the catalog must WAIT rather than
// POLL: polling re-runs its whole reconcile, spending the very rate limit the
// catalog needs to arrive, on every object, for as long as the outage lasts.
func (p *Provider) Refreshed() <-chan struct{} { return p.refreshed }

// Refresh fetches the catalog once.
//
// On failure the previous snapshot is retained and Stale() becomes true. Only
// a total absence of any prior snapshot leaves Get() returning nil.
func (p *Provider) Refresh(ctx context.Context) error {
	serverTypes, err := p.client.ServerTypes(ctx)
	if err != nil {
		p.stale.Store(true)
		metrics.CatalogStale.Set(1)
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
	metrics.CatalogStale.Set(0)
	metrics.CatalogLastSuccess.Set(float64(p.now().Unix()))
	// Hetzner's PUBLISHED availability, exported and used for nothing: graphed
	// beside offering_unavailable it shows where what Hetzner says and what it
	// does disagree. Reset first, or a server type or location that disappears
	// from the catalog keeps its last published value forever.
	metrics.OfferingAvailabilityFlag.Reset()
	for _, st := range serverTypes {
		for _, l := range st.Locations {
			metrics.OfferingAvailabilityFlag.WithLabelValues(st.Name, l.Location).Set(boolToFloat(l.Available))
		}
	}

	select {
	case p.refreshed <- struct{}{}:
	default:
		// A wake-up is already pending. The signal carries no payload, so one
		// covers this refresh too and dropping it keeps the loop non-blocking.
	}
	return nil
}

// Start refreshes until ctx is cancelled. It performs one refresh immediately
// so that a bad token is reported at startup rather than at the first scale-up.
//
// Only a FATAL first refresh is returned. This runs as a manager Runnable, so
// any returned error restarts the pod: treating a Hetzner outage or a 429 as
// fatal would CrashLoopBackOff for its duration and take down the controllers
// needing no Hetzner access. A rejected token will not fix itself.
func (p *Provider) Start(ctx context.Context) error {
	if err := p.Refresh(ctx); err != nil {
		if hcloudapi.Classify(err) == hcloudapi.ClassFatal {
			return err
		}
		log.FromContext(ctx).Error(err, "initial catalog refresh failed, continuing without a catalog")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.nextInterval()):
			// Not returned: the loop keeps serving the last good snapshot.
			// Logged, or a token that silently loses read on server types
			// leaves Get() serving a boot-time snapshot forever.
			if err := p.Refresh(ctx); err != nil {
				log.FromContext(ctx).Error(err, "catalog refresh failed, serving the last good snapshot",
					"class", hcloudapi.Classify(err).String(), "staleFor", p.staleFor())
			}
		}
	}
}

// staleFor renders how long the served snapshot has been stale, for logging.
//
// "never" rather than an age when nothing has ever been fetched, since
// subtracting from the zero time prints a meaningless 2562047h.
func (p *Provider) staleFor() string {
	s := p.snapshot.Load()
	if s == nil {
		return "never fetched"
	}
	return p.now().Sub(s.FetchedAt).Truncate(time.Second).String()
}

func (p *Provider) nextInterval() time.Duration {
	if p.jitter <= 0 {
		return p.interval
	}
	p.randMu.Lock()
	defer p.randMu.Unlock()
	return p.interval + time.Duration(p.rand.Int63n(int64(p.jitter)))
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
