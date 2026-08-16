package instancetype

import (
	"context"
	"time"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/metrics"
)

// SyncInterval is how often the suppression gauge is rebuilt.
//
// Well under the suppression TTL, so a pair that expires is seen as available
// again within one interval rather than lingering on the dashboard.
const SyncInterval = 30 * time.Second

// SyncUnavailable keeps offering_unavailable in step with the suppression cache
// until ctx is cancelled.
//
// Lives here rather than in the metrics package so that package stays a leaf.
// Every other package points AT metrics; having metrics point back at a domain
// package made it the one node in the graph pointing both ways, which is an
// import cycle the first time this package wants to record something itself.
//
// A periodic REBUILD rather than an event on each change, and that is the point.
// Suppressions expire on a timer with nothing to hook, so a gauge only ever set
// on failure stays at 1 long after stock returned. That reads as a permanent
// stockout to whoever is deciding whether capacity has recovered, which is the
// exact question this project exists to answer correctly.
func SyncUnavailable(ctx context.Context, unavailable *Unavailable) error {
	ticker := time.NewTicker(SyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Reset before rebuilding, so a pair whose suppression expired
			// loses its series entirely instead of being stuck at 1 forever.
			metrics.OfferingUnavailable.Reset()
			for _, s := range unavailable.Snapshot() {
				metrics.OfferingUnavailable.WithLabelValues(s.ServerType, s.Location, s.Code).Set(1)
			}
		}
	}
}
