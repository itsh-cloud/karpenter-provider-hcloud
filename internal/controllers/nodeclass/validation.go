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
// needs. Narrowed to an interface so the controller can be tested without a
// catalog refresh loop.
type CatalogProvider interface {
	Get() *catalog.Snapshot
}

// reasonDependenciesNotReady is used for both the False and the Unknown form of
// the dependency short-circuit, so that the two differ only in status.
const reasonDependenciesNotReady = "DependenciesNotReady"

// dependenciesMessage names the conditions actually holding validation back.
//
// Naming them costs nothing in churn: operatorpkg's ConditionSet.Set
// short-circuits on a DeepEqual of the condition, so this rewrites only when
// the failing set genuinely changes. It buys the one thing the roll-up cannot
// say, because karpenter core copies this reason and message verbatim onto the
// NodePool's NodeClassReady, where "awaiting resolution" of six things an
// operator cannot see from there is not a diagnosis.
func dependenciesMessage(failing []string) string {
	return "awaiting " + strings.Join(failing, ", ")
}

// Validation checks the resolved parts of a NodeClass against each other and
// computes the locations a node may actually be placed in.
//
// It must run LAST among the sub-reconcilers. It reports on the six other
// conditions, and it reads status.network.zone that the network reconciler
// resolves in the same pass.
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

	// False beats Unknown. A dependency that has definitively failed makes
	// validation definitively fail, and karpenter core copies this reason and
	// message straight onto the NodePool's NodeClassReady condition, where a
	// False is actionable and an Unknown reads as "the controller is confused".
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
			// this the architecture check below and every conclusion drawn from
			// status would be evaluated against the old spec's resolution and
			// then stamped True at the NEW generation, which reads as "this
			// edit is validated" when nothing has re-resolved it yet. Reached
			// whenever a spec edit coincides with one transient resolver
			// failure, since the transient path deliberately leaves the
			// condition untouched.
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
	// is the only place a debian-13 ARM snapshot pinned onto an x86-only
	// provider is caught. Left uncaught it produces servers that fail to boot,
	// which Hetzner reports as a successful create.
	if img := nodeClass.Status.Image; img != nil && img.Architecture != "" && img.Architecture != SupportedArchitecture {
		nodeClass.Status.Locations = nil
		conds.SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "ImageArchitectureUnsupported",
			fmt.Sprintf("image architecture %q is not %q, which is the only architecture this provider offers instance types for", img.Architecture, SupportedArchitecture))
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	snapshot := v.catalog.Get()
	locations := locationScope(snapshot, nodeClass)

	// Two ways to know nothing about the catalog, and neither is a fact about
	// this NodeClass, so neither may be False. A False here would take every
	// NodeClass in the cluster to Ready=False at once, stop every NodePool and
	// make core delete in-flight NodeClaims, over one bad response from an
	// endpoint the operator does not control.
	//
	// The second case is not hypothetical: ServerTypes drops any location entry
	// whose Location pointer is nil, so a 200 in a shape we did not expect
	// yields a non-nil snapshot with zero usable locations. That is the field
	// Hetzner is changing before 2026-10-01.
	if snapshot == nil || locations.catalogEmpty {
		reason, message := "CatalogNotFetched", "the Hetzner server type catalog has not been fetched yet"
		if snapshot != nil {
			reason, message = "CatalogEmpty", "the Hetzner server type catalog reports no location offering any supported server type"
		}
		nodeClass.Status.Locations = nil
		conds.SetUnknownWithReason(v1alpha1.ConditionTypeValidationSucceeded, reason, message)
		// Short, not misconfiguredRequeue: the expected wait is one catalog
		// fetch, and a minute of Ready=Unknown on every class after every
		// restart and every leader-election failover is a blackout the operator
		// pays for nothing.
		return reconcile.Result{RequeueAfter: catalogRequeue}, nil
	}

	if len(locations.inScope) == 0 {
		nodeClass.Status.Locations = nil
		reason, message := locations.emptyFailure()
		conds.SetFalse(v1alpha1.ConditionTypeValidationSucceeded, reason, message)
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	nodeClass.Status.Locations = locations.inScope
	if excluded := locations.excludedMessage(); excluded != "" {
		// True, not False: the class can still place nodes, and taking the
		// whole NodePool down over one unusable entry in a list of three turns
		// an editing mistake into an outage. But a silently narrowed location
		// set changes the capacity an operator believes they have, so it is
		// said in the reason, in the message, and once more as an event, which
		// is the only one of the three that shows up unprompted.
		//
		// The event is published on transition only. Karpenter's recorder
		// dedupes for two minutes and this reconciler requeues every five, so
		// the dedupe window always expires first: publishing unconditionally
		// bills a stable, deliberately narrowed class ~288 Warning events a day
		// and trains the operator to ignore the one that matters.
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
	// catalogEmpty records that the catalog itself yielded no location at all,
	// which is a fact about the catalog and not about this NodeClass. Kept
	// separate because the two are otherwise indistinguishable from the
	// outcome: with no locations known, every entry in spec.locations lands in
	// unknown, and the failure would blame the operator's spec for an empty
	// upstream response.
	catalogEmpty bool
	// explicit records whether the candidates came from spec.locations.
	//
	// It gates the narrowing warning. Excluding a location the operator never
	// asked for is the zone bound doing its job and is not news; excluding one
	// they wrote down by hand means what they wrote is not what they got.
	explicit bool
	// zone is the resolved private network's zone, or "" when no network is
	// selected. Carried on the scope so the messages below name the same zone
	// the intersection was computed against.
	zone string
}

// locationScope resolves which Hetzner locations this NodeClass may place nodes
// in.
//
// Three bounds intersect here. The catalog says where server types of the
// supported architecture are sold. spec.locations is the operator's
// infrastructure bound. The resolved private network's zone is a hard physical
// bound: a server in one network zone cannot attach to a network in another, so
// a location outside it is not a preference that can be overridden, it simply
// cannot be used.
func locationScope(snapshot *catalog.Snapshot, nodeClass *v1alpha1.HCloudNodeClass) scope {
	zones := map[string]string{}
	if snapshot != nil {
		for _, st := range snapshot.ServerTypes {
			if st.Deprecated || st.Architecture != SupportedArchitecture {
				continue
			}
			for _, l := range st.Locations {
				// A location-scoped deprecation is this type being phased out
				// there, matching how the instance type catalog is built; a
				// location that offers nothing else drops out entirely.
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

	// Deduplicated because the CRD constrains the length and the shape of each
	// entry but not uniqueness, and a repeated entry would otherwise become a
	// repeated status.locations entry and, downstream, a duplicate offering.
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
		case networkZone != "" && zone != networkZone:
			out.outsideZone = append(out.outsideZone, name)
		default:
			out.inScope = append(out.inScope, v1alpha1.LocationStatus{
				Name:        name,
				NetworkZone: zone,
				// Datacenters is deliberately left empty. Hetzner's datacenter
				// API is deprecated and stops being returned after 2026-10-01,
				// and server types are now reported per location rather than
				// per datacenter, so there is nothing truthful to put here.
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
