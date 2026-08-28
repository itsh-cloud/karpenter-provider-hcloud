// Package metrics holds this provider's own Prometheus series: the Hetzner side
// that karpenter core cannot see, since core covers scheduling and disruption.
//
// Every series here has a writer, and that is the rule. A gauge nothing writes
// scrapes as a permanent zero, and for a rate limit zero reads as FULLY
// THROTTLED, so request counts, in-flight depth and the remaining rate limit
// stay undeclared until something wires them.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Namespace and subsystem match karpenter's own convention so the provider's
// series sort next to core's in a dashboard.
const (
	namespace = "karpenter"
	subsystem = "hcloud"
)

var (
	// LaunchFailures counts creates that did not produce a server. The reason
	// label carries this provider's error CLASS, not the raw Hetzner code, so a
	// dashboard need not know Hetzner's vocabulary to show "capacity" separately
	// from "config". The raw code is in the logs.
	LaunchFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "launch_failures_total",
		Help:      "Server creates that failed, by server type, location and error class.",
	}, []string{"server_type", "location", "reason"})

	// LaunchDuration measures a whole Create, including the fall-through.
	// Buckets reach past ninety seconds on purpose: a create that walks several
	// out-of-stock candidates is the interesting case, and a histogram topping
	// out below the action timeout would put every one of them in +Inf.
	LaunchDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "launch_duration_seconds",
		Help:      "Time from the first create attempt to a running server, including fall-through.",
		Buckets:   []float64{5, 10, 20, 30, 45, 60, 90, 120, 180, 300},
	}, []string{"server_type", "location", "result"})

	// OfferingUnavailable is 1 while a (server type, location) pair is
	// suppressed by an observed capacity failure.
	//
	// The label is LOCATION, not datacenter: this provider orders by location,
	// the suppression cache is keyed that way, and Hetzner's datacenter API is
	// deprecated and stops being returned after 2026-10-01.
	OfferingUnavailable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "offering_unavailable",
		Help:      "1 while a (server type, location) pair is suppressed after an observed capacity failure.",
	}, []string{"server_type", "location", "code"})

	// OfferingAvailabilityFlag is what Hetzner PUBLISHES about a pair, exported
	// precisely because nothing decides anything with it. The flag is neither
	// sufficient nor necessary: types reported unavailable have been ordered
	// successfully, and types reported available have returned
	// resource_unavailable. Graphing it beside offering_unavailable makes that
	// divergence visible, which is the only honest use for it.
	OfferingAvailabilityFlag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "offering_published_available",
		Help:      "Hetzner's published availability flag for a (server type, location) pair. Advisory only; nothing gates on it.",
	}, []string{"server_type", "location"})

	// CatalogStale is 1 when the last catalog refresh failed. The snapshot is
	// still served while stale, which is what makes this necessary: without it a
	// token that quietly loses read on server types leaves the provider serving
	// a boot-time catalog forever with every condition reading True.
	CatalogStale = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "catalog_stale",
		Help:      "1 when the most recent Hetzner catalog refresh failed. The last good snapshot is still served.",
	})

	// CatalogLastSuccess is when the served catalog was fetched.
	CatalogLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "catalog_last_success_timestamp_seconds",
		Help:      "Unix time of the last successful Hetzner catalog refresh.",
	})

	// UnknownErrorCodes counts Hetzner error codes this provider does not
	// recognise. They are classified transient and retried, which is the safe
	// default and also a silent one: a new terminal code would be retried
	// forever with nothing saying so.
	UnknownErrorCodes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "unknown_error_codes_total",
		Help:      "Hetzner error codes not recognised by the classifier, retried as transient.",
	}, []string{"code"})

	// OrphansReaped counts servers deleted for having no NodeClaim. Should be
	// zero: a non-zero rate means creates are succeeding at Hetzner and failing
	// to be recorded, which is worth an alert rather than a graph.
	OrphansReaped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      "orphaned_servers_reaped_total",
		Help:      "Servers deleted because they carried this cluster's label but had no NodeClaim.",
	})
)

func init() {
	// controller-runtime's registry is what the operator's /metrics endpoint
	// serves. A separate registry would be collected by nothing.
	crmetrics.Registry.MustRegister(
		LaunchFailures,
		LaunchDuration,
		OfferingUnavailable,
		OfferingAvailabilityFlag,
		CatalogStale,
		CatalogLastSuccess,
		UnknownErrorCodes,
		OrphansReaped,
	)
}
