package nodeclass

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/catalog"
)

// CatalogProvider is the slice of the Hetzner catalog provider this controller
// needs, narrowed so the controller can be tested without a refresh loop.
type CatalogProvider interface {
	Get() *catalog.Snapshot
	// Refreshed yields a value after each successful catalog refresh, so a
	// NodeClass blocked on the catalog is woken rather than polling for it.
	Refreshed() <-chan struct{}
}

// reasonDependenciesNotReady is used for both the False and the Unknown form of
// the dependency short-circuit, so that the two differ only in status.
const reasonDependenciesNotReady = "DependenciesNotReady"

// Reasons for the two ways of knowing nothing about the catalog. Named
// constants because the catalog wake-up filter matches on them, so a typo in
// either place would silently stop waking the classes that need it.
const (
	reasonCatalogNotFetched = "CatalogNotFetched"
	reasonCatalogEmpty      = "CatalogEmpty"
)

// dependenciesMessage names the conditions actually holding validation back.
//
// Karpenter core copies this reason and message verbatim onto the NodePool's
// NodeClassReady, where a bare "awaiting resolution" of six things an operator
// cannot see from there is not a diagnosis. It costs no churn: operatorpkg's
// ConditionSet.Set short-circuits on a DeepEqual of the condition.
func dependenciesMessage(failing []string) string {
	return "awaiting " + strings.Join(failing, ", ")
}

// Validation checks the resolved parts of a NodeClass against each other and
// computes the locations a node may actually be placed in.
//
// It must run LAST: it reports on the six other conditions and reads
// status.network.zone, which the network reconciler resolves in the same pass.
type Validation struct {
	clk      clock.Clock
	recorder events.Recorder
	catalog  CatalogProvider
}

// NewValidation returns the validation sub-reconciler.
func NewValidation(clk clock.Clock, recorder events.Recorder, catalogProvider CatalogProvider) *Validation {
	return &Validation{clk: clk, recorder: recorder, catalog: catalogProvider}
}

// requiredConditions are the conditions validation is a function of.
func requiredConditions() []string {
	return []string{
		v1alpha1.ConditionTypeImageReady,
		v1alpha1.ConditionTypeNetworkReady,
		v1alpha1.ConditionTypeFirewallsReady,
		v1alpha1.ConditionTypeSSHKeysReady,
		v1alpha1.ConditionTypePlacementGroupReady,
		v1alpha1.ConditionTypeBootstrapDiscoveryReady,
	}
}

func (v *Validation) Reconcile(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	conds := nodeClass.StatusConditions(status.WithClock(v.clk))

	// False beats Unknown. Core copies this straight onto the NodePool's
	// NodeClassReady, where a False is actionable and an Unknown reads as "the
	// controller is confused".
	var failed, pending []string
	for _, name := range requiredConditions() {
		cond := conds.Get(name)
		switch {
		case cond.IsFalse():
			failed = append(failed, name)
		case cond.IsUnknown():
			pending = append(pending, name)
		case cond.ObservedGeneration != nodeClass.Generation:
			// Resolved, but against a previous revision of the spec. Without
			// this, conclusions drawn from the old resolution get stamped True
			// at the NEW generation, reading as "this edit is validated" when
			// nothing has re-resolved it. Reached whenever a spec edit coincides
			// with a transient resolver failure, which leaves conditions alone.
			pending = append(pending, name)
		}
	}
	if len(failed) > 0 {
		nodeClass.Status.Locations = nil
		conds.SetFalse(v1alpha1.ConditionTypeValidationSucceeded, reasonDependenciesNotReady, dependenciesMessage(failed))
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}
	if len(pending) > 0 {
		nodeClass.Status.Locations = nil
		conds.SetUnknownWithReason(v1alpha1.ConditionTypeValidationSucceeded, reasonDependenciesNotReady, dependenciesMessage(pending))
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	// An id-pinned image is not architecture-qualified by the resolver, so this
	// is the only place an ARM snapshot pinned onto an x86-only provider is
	// caught. Uncaught it produces servers that fail to boot, which Hetzner
	// reports as a successful create.
	if img := nodeClass.Status.Image; img != nil && img.Architecture != "" && img.Architecture != SupportedArchitecture {
		nodeClass.Status.Locations = nil
		conds.SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "ImageArchitectureUnsupported",
			fmt.Sprintf("image architecture %q is not %q, which is the only architecture this provider offers instance types for", img.Architecture, SupportedArchitecture))
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	snapshot := v.catalog.Get()
	locations := locationScope(snapshot, nodeClass)

	// Neither way of knowing nothing about the catalog is a fact about this
	// NodeClass, so neither may be False: a False takes every NodeClass in the
	// cluster to Ready=False at once, stops every NodePool and makes core delete
	// in-flight NodeClaims, over one bad response from an endpoint the operator
	// does not control. The empty case is reachable: ServerTypes drops any
	// location entry whose Location pointer is nil, which is the field Hetzner
	// is changing before 2026-10-01.
	if snapshot == nil || locations.catalogEmpty {
		reason, message := reasonCatalogNotFetched, "the Hetzner server type catalog has not been fetched yet"
		if snapshot != nil {
			reason, message = reasonCatalogEmpty, "the Hetzner server type catalog reports no location offering any supported server type"
		}
		nodeClass.Status.Locations = nil
		conds.SetUnknownWithReason(v1alpha1.ConditionTypeValidationSucceeded, reason, message)
		// A SLOW backstop, not a poll: this branch cannot fix itself by being
		// re-run, and nothing throttles RequeueAfter, which routes through
		// Queue.Forget and AddAfter and consults neither the exponential limiter
		// nor the token bucket. Five Hetzner GETs every five seconds from one
		// NodeClass is the entire per-project limit, shared with the CCM and the
		// CSI driver, and both triggers can persist indefinitely. Promptness
		// comes from the Refreshed() watch in Register instead.
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	if len(locations.inScope) == 0 {
		nodeClass.Status.Locations = nil
		reason, message := locations.emptyFailure()
		conds.SetFalse(v1alpha1.ConditionTypeValidationSucceeded, reason, message)
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	nodeClass.Status.Locations = locations.inScope
	if excluded := locations.excludedMessage(); excluded != "" {
		// True, not False: the class can still place nodes, and taking the whole
		// NodePool down over one unusable entry turns an editing mistake into an
		// outage. The narrowing is still said in the reason, the message and an
		// event, the last being the only one that shows up unprompted. On
		// transition only: the recorder's dedupe window is two minutes and this
		// requeues every five, so publishing every pass is ~288 events a day.
		prev := conds.Get(v1alpha1.ConditionTypeValidationSucceeded)
		firstTime := prev == nil || prev.Reason != "LocationsNarrowed" || prev.Message != excluded
		conds.SetTrueWithReason(v1alpha1.ConditionTypeValidationSucceeded, "LocationsNarrowed", excluded)
		if firstTime {
			v.recorder.Publish(LocationsNarrowedEvent(nodeClass, excluded))
		}
	} else {
		conds.SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
	}
	return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
}

// scope is the outcome of intersecting spec.locations with the catalog and the
// private network's zone.
type scope struct {
	inScope []v1alpha1.LocationStatus
	// outsideZone are candidate locations the private network cannot reach.
	outsideZone []string
	// unknown are candidate locations no server type is offered in.
	unknown []string
	// catalogEmpty records that the catalog yielded no location at all, a fact
	// about the catalog and not about this NodeClass. Kept separate because
	// otherwise every entry in spec.locations lands in unknown and the failure
	// blames the operator's spec for an empty upstream response.
	catalogEmpty bool
	// explicit records whether the candidates came from spec.locations. It gates
	// the narrowing warning: excluding a location nobody asked for is the zone
	// bound doing its job, excluding one they wrote down is news.
	explicit bool
	// zone is the resolved private network's zone, or "" when no network is
	// selected. Carried here so the messages below name the same zone the
	// intersection was computed against.
	zone string
}

// locationScope resolves which Hetzner locations this NodeClass may place nodes
// in.
//
// Three bounds intersect: the catalog (where supported server types are sold),
// spec.locations (the operator's bound), and the private network's zone, which
// is physical rather than a preference, because a server cannot attach to a
// network in another zone.
func locationScope(snapshot *catalog.Snapshot, nodeClass *v1alpha1.HCloudNodeClass) scope {
	zones := map[string]string{}
	if snapshot != nil {
		for _, st := range snapshot.ServerTypes {
			if st.Deprecated || st.Architecture != SupportedArchitecture {
				continue
			}
			for _, l := range st.Locations {
				// A location-scoped deprecation is this type being phased out
				// there, matching how the instance type catalog is built.
				if l.Deprecated {
					continue
				}
				zones[l.Location] = l.NetworkZone
			}
		}
	}

	networkZone := ""
	if nodeClass.Status.Network != nil {
		networkZone = nodeClass.Status.Network.Zone
	}

	out := scope{
		explicit:     len(nodeClass.Spec.Locations) > 0,
		catalogEmpty: len(zones) == 0,
		zone:         networkZone,
	}
	names := nodeClass.Spec.Locations
	if len(names) == 0 {
		names = make([]string, 0, len(zones))
		for name := range zones {
			names = append(names, name)
		}
		// Sorted because map iteration is randomised, and an unsorted
		// status.locations would rewrite itself on every reconcile.
		sort.Strings(names)
	}

	// Deduplicated because the CRD constrains each entry's shape but not
	// uniqueness, and a repeat becomes a duplicate offering downstream.
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		zone, offered := zones[name]
		switch {
		case !offered:
			out.unknown = append(out.unknown, name)
		// Excluded only when the location's zone is KNOWN and differs. The
		// bare zone != networkZone test treats an unknown zone as a proven
		// mismatch, and the two mistakes cost wildly differently: wrongly
		// excluding drops every location, wrongly including surfaces as one
		// create failing with an error that names the network.
		case zone != "" && networkZone != "" && zone != networkZone:
			out.outsideZone = append(out.outsideZone, name)
		default:
			out.inScope = append(out.inScope, v1alpha1.LocationStatus{
				Name:        name,
				NetworkZone: zone,
				// Datacenters is deliberately empty: Hetzner's datacenter
				// API is deprecated and stops being returned after
				// 2026-10-01, and server types are reported per location.
			})
		}
	}
	return out
}

// emptyFailure explains an empty location set. The reason distinguishes the
// three causes because the fixes are different: widen the network, fix a typo,
// or look at why the catalog is empty.
func (s scope) emptyFailure() (string, string) {
	switch {
	case len(s.outsideZone) > 0 && len(s.unknown) > 0:
		return "NoUsableLocations", fmt.Sprintf(
			"no location is usable: %s are outside the private network's zone %q, and %s are not offered by any supported server type",
			strings.Join(s.outsideZone, ", "), s.zone, strings.Join(s.unknown, ", "))
	case len(s.outsideZone) > 0:
		return "LocationsOutsideNetworkZone", fmt.Sprintf(
			"every location in scope (%s) is outside the private network's zone %q, and a server cannot attach to a network in another zone",
			strings.Join(s.outsideZone, ", "), s.zone)
	case len(s.unknown) > 0:
		return "UnknownLocations", fmt.Sprintf(
			"no supported server type is offered in any location in scope (%s)",
			strings.Join(s.unknown, ", "))
	default:
		return "NoLocationsAvailable", "the Hetzner catalog offers no location for any supported server type"
	}
}

// excludedMessage describes a partially narrowed location set, or "" when
// nothing was dropped.
func (s scope) excludedMessage() string {
	if !s.explicit || (len(s.outsideZone) == 0 && len(s.unknown) == 0) {
		return ""
	}
	var parts []string
	if len(s.outsideZone) > 0 {
		parts = append(parts, fmt.Sprintf("%s outside the private network's zone %q", strings.Join(s.outsideZone, ", "), s.zone))
	}
	if len(s.unknown) > 0 {
		parts = append(parts, fmt.Sprintf("%s not offered by any supported server type", strings.Join(s.unknown, ", ")))
	}
	return "spec.locations was narrowed, dropping " + strings.Join(parts, " and ")
}
