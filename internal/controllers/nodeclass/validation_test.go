package nodeclass

import (
	"context"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/catalog"
)

func locationNames(nc *v1alpha1.HCloudNodeClass) []string {
	out := make([]string, 0, len(nc.Status.Locations))
	for _, l := range nc.Status.Locations {
		out = append(out, l.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLocationScope is the heart of the location logic. Three bounds intersect:
// what the catalog sells for the supported architecture, what spec.locations
// permits, and what the private network's zone can physically reach. The last
// is not a preference, it is a hard constraint: a server in one network zone
// cannot attach to a network in another, so a location outside it produces a
// create that fails every time.
func TestLocationScope(t *testing.T) {
	for _, tc := range []struct {
		name          string
		specLocations []string
		networkZone   string
		wantLocations []string
		wantStatus    metav1.ConditionStatus
		wantReason    string
		wantEvent     bool
	}{
		{
			name:          "unrestrictedInsideZone",
			networkZone:   "eu-central",
			wantLocations: []string{"fsn1", "nbg1"},
			wantStatus:    metav1.ConditionTrue,
			wantReason:    v1alpha1.ConditionTypeValidationSucceeded,
		},
		{
			name:          "noNetworkMeansNoZoneBound",
			wantLocations: []string{"fsn1", "hil", "nbg1"},
			wantStatus:    metav1.ConditionTrue,
			wantReason:    v1alpha1.ConditionTypeValidationSucceeded,
		},
		{
			name:          "specNarrowsFurther",
			specLocations: []string{"nbg1"},
			networkZone:   "eu-central",
			wantLocations: []string{"nbg1"},
			wantStatus:    metav1.ConditionTrue,
			wantReason:    v1alpha1.ConditionTypeValidationSucceeded,
		},
		{
			// Partial exclusion stays usable. Taking the whole NodePool down
			// over one bad entry in a list of two turns an editing mistake into
			// an outage, so the narrowing is reported rather than enforced.
			name:          "partiallyOutsideZoneIsNarrowedNotFailed",
			specLocations: []string{"nbg1", "hil"},
			networkZone:   "eu-central",
			wantLocations: []string{"nbg1"},
			wantStatus:    metav1.ConditionTrue,
			wantReason:    "LocationsNarrowed",
			wantEvent:     true,
		},
		{
			name:          "typoIsNarrowed",
			specLocations: []string{"nbg1", "nbg9"},
			networkZone:   "eu-central",
			wantLocations: []string{"nbg1"},
			wantStatus:    metav1.ConditionTrue,
			wantReason:    "LocationsNarrowed",
			wantEvent:     true,
		},
		{
			// Nothing usable is a hard failure: every create would fail, so
			// blocking provisioning is strictly better than discovering it one
			// NodeClaim at a time.
			name:          "entirelyOutsideZone",
			specLocations: []string{"hil"},
			networkZone:   "eu-central",
			wantStatus:    metav1.ConditionFalse,
			wantReason:    "LocationsOutsideNetworkZone",
		},
		{
			name:          "entirelyUnknown",
			specLocations: []string{"nbg9"},
			networkZone:   "eu-central",
			wantStatus:    metav1.ConditionFalse,
			wantReason:    "UnknownLocations",
		},
		{
			name:          "mixedCauses",
			specLocations: []string{"hil", "nbg9"},
			networkZone:   "eu-central",
			wantStatus:    metav1.ConditionFalse,
			wantReason:    "NoUsableLocations",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			nodeClass := newNodeClass()
			nodeClass.Spec.Locations = tc.specLocations
			if tc.networkZone == "" {
				nodeClass.Spec.NetworkSelector = nil
			}
			h := newHarness(t, nodeClass)
			if tc.networkZone != "" {
				h.resources.network = func() (*hcloudapi.Network, error) {
					return &hcloudapi.Network{ID: 7, Name: "cluster", IPRange: "10.0.0.0/16", Zone: tc.networkZone}, nil
				}
			}

			nc, err := h.reconcile(ctx)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			assertCondition(t, nc, v1alpha1.ConditionTypeValidationSucceeded, tc.wantStatus, tc.wantReason)
			if got := locationNames(nc); !equalStrings(got, tc.wantLocations) {
				t.Errorf("status.locations = %v, want %v", got, tc.wantLocations)
			}

			narrowed := false
			for _, r := range h.recorder.reasons() {
				if r == "LocationsNarrowed" {
					narrowed = true
				}
			}
			if narrowed != tc.wantEvent {
				t.Errorf("LocationsNarrowed event published = %v, want %v", narrowed, tc.wantEvent)
			}
		})
	}
}

// TestCatalogNotFetchedIsUnknownNotFalse: for the first seconds after every
// restart nothing is known about the catalog. Reporting that as a
// configuration failure would put every NodeClass into Ready=False, which
// karpenter core acts on by deleting in-flight NodeClaims.
func TestCatalogNotFetchedIsUnknownNotFalse(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())
	h.catalog.snapshot = nil

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertCondition(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionUnknown, "CatalogNotFetched")
	assertCondition(t, nc, status.ConditionReady, metav1.ConditionUnknown, "ReconcilingDependents")
}

// TestImageArchitectureMismatch: a name-based image lookup is
// architecture-qualified by the resolver, but an id-pinned one is not, so this
// is the only place a snapshot for the wrong architecture is caught. Hetzner
// reports the create as successful and the server simply never boots.
func TestImageArchitectureMismatch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())
	h.resources.image = func() (*hcloudapi.Image, error) {
		return &hcloudapi.Image{ID: 99, Name: "debian-13", Architecture: "arm"}, nil
	}

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// ImageReady is True: the image resolved. It is the combination that is
	// wrong, which is what validation is for.
	assertCondition(t, nc, v1alpha1.ConditionTypeImageReady, metav1.ConditionTrue, "")
	assertCondition(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, "ImageArchitectureUnsupported")
	assertCondition(t, nc, status.ConditionReady, metav1.ConditionFalse, "UnhealthyDependents")
}

// TestLocationsIgnoreOtherArchitectures: the catalog contains arm types whose
// location set differs, and status.locations must describe where a node this
// provider can actually build may be placed, not where Hetzner sells anything.
func TestLocationsIgnoreOtherArchitectures(t *testing.T) {
	ctx := context.Background()
	nodeClass := newNodeClass()
	nodeClass.Spec.Locations = nil
	h := newHarness(t, nodeClass)
	h.catalog.snapshot = &catalog.Snapshot{
		Generation: 1,
		ServerTypes: []hcloudapi.ServerType{
			{Name: "cx23", Architecture: "x86", Locations: []hcloudapi.ServerTypeLocation{
				{Location: "nbg1", NetworkZone: "eu-central"},
			}},
			{Name: "cax11", Architecture: "arm", Locations: []hcloudapi.ServerTypeLocation{
				{Location: "hel1", NetworkZone: "eu-central"},
			}},
			// A type being phased out in one location leaves that location with
			// nothing on offer, matching how the instance type catalog is built.
			{Name: "cx11", Architecture: "x86", Locations: []hcloudapi.ServerTypeLocation{
				{Location: "ash", NetworkZone: "eu-central", Deprecated: true},
			}},
		},
	}

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := locationNames(nc); !equalStrings(got, []string{"nbg1"}) {
		t.Errorf("status.locations = %v, want only nbg1", got)
	}
}

// TestNetworkWithoutSubnetFails: a network with no subnet has no zone, and no
// server can attach to it. Caught here rather than at create time, where it
// surfaces as a generic invalid_input on every single NodeClaim.
func TestNetworkWithoutSubnetFails(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())
	h.resources.network = func() (*hcloudapi.Network, error) {
		return &hcloudapi.Network{ID: 7, Name: "cluster", IPRange: "10.0.0.0/16"}, nil
	}

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertCondition(t, nc, v1alpha1.ConditionTypeNetworkReady, metav1.ConditionFalse, "NetworkHasNoSubnet")
	assertCondition(t, nc, status.ConditionReady, metav1.ConditionFalse, "UnhealthyDependents")
}

// TestFirewallPartialResolutionFailsClosed: publishing the firewalls that did
// resolve would launch nodes with a subset of the intended rules, silently
// leaving open whatever the missing firewall was there to close.
func TestFirewallPartialResolutionFailsClosed(t *testing.T) {
	ctx := context.Background()
	nodeClass := newNodeClass()
	nodeClass.Spec.FirewallSelectors = []v1alpha1.FirewallSelectorTerm{{Name: "nodes"}, {Name: "gone"}}
	h := newHarness(t, nodeClass)
	h.resources.firewall = func(name string) (*hcloudapi.Firewall, error) {
		if name == "gone" {
			return nil, &hcloudapi.NotFoundError{Kind: "firewall", Selector: name}
		}
		return &hcloudapi.Firewall{ID: 1, Name: name}, nil
	}

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertCondition(t, nc, v1alpha1.ConditionTypeFirewallsReady, metav1.ConditionFalse, "FirewallNotFound")
	if nc.Status.Firewalls != nil {
		t.Errorf("status.firewalls = %v, want nil: a partial firewall set must not be published", nc.Status.Firewalls)
	}
}

// TestFirewallTransportErrorMidListIsRetried: one call failing for transport
// reasons must not be mistaken for the firewall being absent.
func TestFirewallTransportErrorMidListIsRetried(t *testing.T) {
	ctx := context.Background()
	nodeClass := newNodeClass()
	nodeClass.Spec.FirewallSelectors = []v1alpha1.FirewallSelectorTerm{{Name: "nodes"}, {Name: "flaky"}}
	h := newHarness(t, nodeClass)
	h.resources.firewall = func(name string) (*hcloudapi.Firewall, error) {
		if name == "flaky" {
			return nil, transientErr
		}
		return &hcloudapi.Firewall{ID: 1, Name: name}, nil
	}

	if _, err := h.reconcile(ctx); err == nil {
		t.Fatal("reconcile returned nil for a transport failure")
	}
	var nc v1alpha1.HCloudNodeClass
	if err := h.client.Get(ctx, h.name, &nc); err != nil {
		t.Fatalf("get: %v", err)
	}
	assertCondition(t, &nc, v1alpha1.ConditionTypeFirewallsReady, metav1.ConditionUnknown, "")
}

// TestLocationsAreNotJudgedAgainstAnEmptyZone.
//
// A regression test for a bug that no unit test could have caught, because the
// fixtures all supplied a NetworkZone the real API does not return.
//
// /v1/server_types embeds a location object carrying only id, name,
// recommended, available and deprecation. There is no network_zone, so reading
// it from there produced "" for every location. Against a NodeClass whose
// private network really is in eu-central, every location then compared
// unequal and validation reported that nbg1 and fsn1 were "outside the private
// network's zone", which is both false and unactionable.
//
// The zone now comes from /v1/locations. This test pins the consequence rather
// than the plumbing: a catalog that reports no zone must never be read as
// "every location is in the wrong zone".
func TestLocationsAreNotJudgedAgainstAnEmptyZone(t *testing.T) {
	nodeClass := &v1alpha1.HCloudNodeClass{
		Spec: v1alpha1.HCloudNodeClassSpec{Locations: []string{"nbg1", "fsn1"}},
	}
	nodeClass.Status.Network = &v1alpha1.NetworkStatus{Name: "k8s-network", Zone: "eu-central"}

	snapshot := &catalog.Snapshot{ServerTypes: []hcloudapi.ServerType{{
		Name:         "cx43",
		Architecture: SupportedArchitecture,
		Locations: []hcloudapi.ServerTypeLocation{
			// The shape the bug produced: a real location, no zone.
			{Location: "nbg1", NetworkZone: ""},
			{Location: "fsn1", NetworkZone: ""},
		},
	}}}

	got := locationScope(snapshot, nodeClass)
	if len(got.outsideZone) > 0 {
		t.Errorf("locations %v reported outside the network zone on the strength of an empty zone; "+
			"an unknown zone is not evidence of a mismatch", got.outsideZone)
	}
}

// TestNetworkZoneBoundStillApplies is the other half: once the zone is really
// known, a location in a different one genuinely cannot be used, because a
// server cannot attach to a private network across zones.
func TestNetworkZoneBoundStillApplies(t *testing.T) {
	nodeClass := &v1alpha1.HCloudNodeClass{
		Spec: v1alpha1.HCloudNodeClassSpec{Locations: []string{"nbg1", "hil"}},
	}
	nodeClass.Status.Network = &v1alpha1.NetworkStatus{Name: "k8s-network", Zone: "eu-central"}

	snapshot := &catalog.Snapshot{ServerTypes: []hcloudapi.ServerType{{
		Name:         "cx43",
		Architecture: SupportedArchitecture,
		Locations: []hcloudapi.ServerTypeLocation{
			{Location: "nbg1", NetworkZone: "eu-central"},
			{Location: "hil", NetworkZone: "us-west"},
		},
	}}}

	got := locationScope(snapshot, nodeClass)
	if len(got.inScope) != 1 || got.inScope[0].Name != "nbg1" {
		t.Errorf("inScope = %+v, want only nbg1", got.inScope)
	}
	if len(got.outsideZone) != 1 || got.outsideZone[0] != "hil" {
		t.Errorf("outsideZone = %v, want only hil", got.outsideZone)
	}
}
