package cloudprovider

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// resolvedNodeClass is a class whose status matches the server built below, so
// each test only has to introduce the ONE difference it is about.
func resolvedNodeClass() *v1alpha1.HCloudNodeClass {
	nc := &v1alpha1.HCloudNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nc.Status.Image = &v1alpha1.ImageStatus{ID: 42, Name: "debian-13"}
	nc.Status.Network = &v1alpha1.NetworkStatus{ID: 7, Name: "k8s-network", Zone: "eu-central"}
	nc.Status.Firewalls = []v1alpha1.FirewallStatus{{ID: 1, Name: "k8s-fw"}}
	nc.Status.Locations = []v1alpha1.LocationStatus{{Name: "nbg1", NetworkZone: "eu-central"}}
	// Written down by the operator. Location drift only applies to an explicit
	// list; see TestCatalogNarrowingIsNotLocationDrift for why.
	nc.Spec.Locations = []string{"nbg1"}
	return nc
}

func matchingServer() *hcloudapi.Server {
	return &hcloudapi.Server{
		ID: 1, Name: "worker", ServerType: "cx23", Location: "nbg1",
		ImageID: 42, NetworkIDs: []int64{7}, FirewallIDs: []int64{1},
		// Both true, because both spec fields default to true and a server
		// built from a default NodeClass really does get them. A fixture
		// without them is not a matching server, it is a drifted one.
		HasPublicIPv4: true, HasPublicIPv6: true,
	}
}

// TestNoDriftWhenNothingChanged is the case that matters most: this runs
// against every node continuously, and a false positive replaces the fleet.
func TestNoDriftWhenNothingChanged(t *testing.T) {
	if got := serverDrift(resolvedNodeClass(), matchingServer()); got != "" {
		t.Errorf("serverDrift = %q for a server that matches its NodeClass exactly", got)
	}
}

func TestServerDriftDetectsRealChanges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*v1alpha1.HCloudNodeClass, *hcloudapi.Server)
		expected cloudprovider.DriftReason
	}{
		{"locationRemovedFromClass", func(nc *v1alpha1.HCloudNodeClass, _ *hcloudapi.Server) {
			nc.Spec.Locations = []string{"fsn1"}
			nc.Status.Locations = []v1alpha1.LocationStatus{{Name: "fsn1", NetworkZone: "eu-central"}}
		}, LocationDrift},
		{"networkChanged", func(nc *v1alpha1.HCloudNodeClass, _ *hcloudapi.Server) {
			nc.Status.Network = &v1alpha1.NetworkStatus{ID: 99, Name: "other", Zone: "eu-central"}
		}, NetworkDrift},
		{"networkRemovedFromClass", func(nc *v1alpha1.HCloudNodeClass, _ *hcloudapi.Server) {
			nc.Status.Network = nil
		}, NetworkDrift},
		{"firewallAdded", func(nc *v1alpha1.HCloudNodeClass, _ *hcloudapi.Server) {
			nc.Status.Firewalls = append(nc.Status.Firewalls, v1alpha1.FirewallStatus{ID: 2, Name: "extra"})
		}, FirewallDrift},
		{"firewallRemoved", func(nc *v1alpha1.HCloudNodeClass, _ *hcloudapi.Server) {
			nc.Status.Firewalls = nil
		}, FirewallDrift},
		{"placementGroupAdded", func(nc *v1alpha1.HCloudNodeClass, _ *hcloudapi.Server) {
			nc.Status.PlacementGroup = &v1alpha1.PlacementGroupStatus{ID: 3, Name: "spread"}
		}, PlacementGroupDrift},
		{"placementGroupRemoved", func(nc *v1alpha1.HCloudNodeClass, srv *hcloudapi.Server) {
			srv.PlacementGroupID = 3
		}, PlacementGroupDrift},
		{"publicIPv4Disabled", func(nc *v1alpha1.HCloudNodeClass, _ *hcloudapi.Server) {
			nc.Spec.PublicIPv4 = ptr(false)
		}, PublicNetDrift},
		{"publicIPv6Disabled", func(nc *v1alpha1.HCloudNodeClass, _ *hcloudapi.Server) {
			nc.Spec.PublicIPv6 = ptr(false)
		}, PublicNetDrift},
		{"serverLostItsPublicIPv4", func(_ *v1alpha1.HCloudNodeClass, srv *hcloudapi.Server) {
			srv.HasPublicIPv4 = false
		}, PublicNetDrift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc, srv := resolvedNodeClass(), matchingServer()
			tc.mutate(nc, srv)
			if got := serverDrift(nc, srv); got != tc.expected {
				t.Errorf("serverDrift = %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestFirewallDriftIgnoresOrder: hcloud returns firewalls in its own order and
// an operator can reorder selectors without meaning anything by it. Comparing
// as a sequence would mark healthy nodes drifted on a cosmetic edit.
func TestFirewallDriftIgnoresOrder(t *testing.T) {
	nc := resolvedNodeClass()
	nc.Status.Firewalls = []v1alpha1.FirewallStatus{{ID: 1}, {ID: 2}, {ID: 3}}
	srv := matchingServer()
	srv.FirewallIDs = []int64{3, 1, 2}

	if got := serverDrift(nc, srv); got != "" {
		t.Errorf("serverDrift = %q for the same firewall set in a different order", got)
	}
}

// TestServerTypeIsNeverDrift.
//
// Deliberate and load-bearing. Which machine shape a workload sits on is a
// scheduling decision, and correcting a wrongly-typed node is consolidation's
// job: it can simulate the replacement, confirm the pods still fit and the
// price is lower, and act. Treating the type as drift would have this
// controller fighting consolidation over the same node, which is the churn this
// project exists to remove.
func TestServerTypeIsNeverDrift(t *testing.T) {
	nc := resolvedNodeClass()
	srv := matchingServer()
	srv.ServerType = "cpx52" // wildly different, and wildly more expensive

	if got := serverDrift(nc, srv); got != "" {
		t.Errorf("serverDrift = %q on a server type mismatch; that is consolidation's decision, not drift's", got)
	}
}

// TestImageDriftHonoursThePolicy.
//
// Hetzner rebuilds its named images every few weeks, so the id behind a name
// changes on Hetzner's schedule. Under the default Ignore that must not roll
// the fleet, or the operator gets an unplanned full replacement whenever
// Hetzner publishes.
func TestImageDriftHonoursThePolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   v1alpha1.ImageDriftPolicy
		expected cloudprovider.DriftReason
	}{
		{"defaultIgnores", "", ""},
		{"explicitIgnore", v1alpha1.ImageDriftPolicyIgnore, ""},
		{"replaceDrifts", v1alpha1.ImageDriftPolicyReplace, ImageDrift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc := resolvedNodeClass()
			nc.Spec.ImageDriftPolicy = tc.policy
			srv := matchingServer()
			srv.ImageID = 999 // Hetzner rebuilt the image

			if got := serverDrift(nc, srv); got != tc.expected {
				t.Errorf("serverDrift = %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestHashDriftRequiresMatchingVersions.
//
// The single most dangerous case in the whole drift design. A changed hash
// generator produces a different hash for an unchanged spec, so comparing
// across versions reads EVERY node as drifted and replaces the entire fleet at
// once. The hash controller back-fills NodeClaims to the new version first;
// until both sides agree, this must decline to judge.
func TestHashDriftRequiresMatchingVersions(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		classVersion, claimVer string
		classHash, claimHash   string
		expected               cloudprovider.DriftReason
	}{
		{"sameVersionSameHash", v1alpha1.HashVersion, v1alpha1.HashVersion, "abc", "abc", ""},
		{"sameVersionDifferentHash", v1alpha1.HashVersion, v1alpha1.HashVersion, "abc", "xyz", NodeClassDrift},
		// The fleet-roll cases. Different hashes, but the versions disagree, so
		// the hashes are not comparable and no judgement may be made.
		{"classAheadOfClaim", v1alpha1.HashVersion, "v0", "abc", "xyz", ""},
		{"claimAheadOfClass", "v0", v1alpha1.HashVersion, "abc", "xyz", ""},
		{"bothStale", "v0", "v0", "abc", "xyz", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc := resolvedNodeClass()
			nc.Annotations = map[string]string{
				v1alpha1.AnnotationHashVersion: tc.classVersion,
				v1alpha1.AnnotationHash:        tc.classHash,
			}
			claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: "n1",
				Annotations: map[string]string{
					v1alpha1.AnnotationHashVersion: tc.claimVer,
					v1alpha1.AnnotationHash:        tc.claimHash,
				},
			}}
			if got := nodeClassHashDrift(context.Background(), nc, claim); got != tc.expected {
				t.Errorf("nodeClassHashDrift = %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestHashDriftToleratesAnUnstampedNodeClaim: absence is not evidence. A
// NodeClaim the hash controller has not reached yet must not be replaced for
// it.
func TestHashDriftToleratesAnUnstampedNodeClaim(t *testing.T) {
	nc := resolvedNodeClass()
	nc.Annotations = map[string]string{
		v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion,
		v1alpha1.AnnotationHash:        "abc",
	}
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:        "n1",
		Annotations: map[string]string{v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion},
	}}
	if got := nodeClassHashDrift(context.Background(), nc, claim); got != "" {
		t.Errorf("nodeClassHashDrift = %q for a NodeClaim with no hash yet", got)
	}
}

// TestIsDriftedIgnoresAnUnlaunchedNodeClaim: reporting drift for a NodeClaim
// with no server would ask core to replace something that does not exist.
func TestIsDriftedIgnoresAnUnlaunchedNodeClaim(t *testing.T) {
	cp, _ := newTestProvider(t)
	got, err := cp.IsDrifted(context.Background(), &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "n1"}})
	if err != nil || got != "" {
		t.Errorf("IsDrifted = %q, %v for a NodeClaim that never launched", got, err)
	}
}

// TestIsDriftedIgnoresAVanishedServer: that is the garbage collector's
// business. Replacing here would create a second server for a NodeClaim whose
// first one already went away.
func TestIsDriftedIgnoresAVanishedServer(t *testing.T) {
	nc := resolvedNodeClass()
	cp, _ := newTestProvider(t, nc)

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default"}},
		Status:     karpv1.NodeClaimStatus{ProviderID: "hcloud://404"},
	}
	got, err := cp.IsDrifted(context.Background(), claim)
	if err != nil || got != "" {
		t.Errorf("IsDrifted = %q, %v for a server that no longer exists", got, err)
	}
}

// TestIsDriftedIgnoresATerminatingNodeClass: it is already being removed, and
// declaring its nodes drifted races the termination gate for no benefit.
func TestIsDriftedIgnoresATerminatingNodeClass(t *testing.T) {
	nc := resolvedNodeClass()
	now := metav1.Now()
	nc.DeletionTimestamp = &now
	nc.Finalizers = []string{v1alpha1.TerminationFinalizer}
	nc.Annotations = map[string]string{
		v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion,
		v1alpha1.AnnotationHash:        "class-hash",
	}
	cp, servers := newTestProvider(t, nc)
	servers.byID[1] = matchingServer()
	servers.byID[1].Labels = map[string]string{hcloudapi.LabelManagedBy: testCluster}

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Annotations: map[string]string{
			v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion,
			v1alpha1.AnnotationHash:        "a-different-hash",
		}},
		Spec:   karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default"}},
		Status: karpv1.NodeClaimStatus{ProviderID: "hcloud://1"},
	}
	got, err := cp.IsDrifted(context.Background(), claim)
	if err != nil || got != "" {
		t.Errorf("IsDrifted = %q, %v for a terminating NodeClass", got, err)
	}
}

func ptr[T any](v T) *T { return &v }

// TestCatalogNarrowingIsNotLocationDrift.
//
// With spec.locations unset, status.locations is derived ENTIRELY from the
// Hetzner catalog. A partial catalog response, or a server type going
// deprecated in one location, silently narrows it, and the narrowing warning is
// gated on the list being explicit so ValidationSucceeded stays True with no
// event and no message.
//
// Treating that as drift would replace every node in a location on the strength
// of one odd API response, with the NodeClass reporting Ready the whole time.
// An operator removing a location is intent; the catalog moving underneath us
// is not.
func TestCatalogNarrowingIsNotLocationDrift(t *testing.T) {
	nc := resolvedNodeClass()
	nc.Spec.Locations = nil // the operator never wrote a list
	// The catalog no longer reports nbg1, where this server lives.
	nc.Status.Locations = []v1alpha1.LocationStatus{{Name: "fsn1", NetworkZone: "eu-central"}}

	if got := serverDrift(nc, matchingServer()); got != "" {
		t.Errorf("serverDrift = %q from a catalog-narrowed location set; that would replace every node in a location "+
			"because one API response came back short", got)
	}
}

// TestExplicitLocationRemovalStillDrifts is the other half, so the fix above
// cannot be satisfied by disabling location drift entirely.
func TestExplicitLocationRemovalStillDrifts(t *testing.T) {
	nc := resolvedNodeClass()
	nc.Spec.Locations = []string{"fsn1"}
	nc.Status.Locations = []v1alpha1.LocationStatus{{Name: "fsn1", NetworkZone: "eu-central"}}

	if got := serverDrift(nc, matchingServer()); got != LocationDrift {
		t.Errorf("serverDrift = %q, want LocationDrift: the operator removed nbg1 from spec.locations", got)
	}
}

// TestDriftDeclinesOnAnUnreadyNodeClass.
//
// Every comparison reads nodeClass.Status, and the resolvers NIL those fields
// when they cannot resolve: a renamed firewall empties Status.Firewalls. Judging
// against that reads as "everything changed" and marks the whole fleet drifted
// on one bad selector.
func TestDriftDeclinesOnAnUnreadyNodeClass(t *testing.T) {
	nc := resolvedNodeClass()
	// What a renamed firewall does to the status.
	nc.Status.Firewalls = nil
	nc.StatusConditions().SetFalse(v1alpha1.ConditionTypeFirewallsReady, "FirewallNotFound", "no such firewall")

	cp, servers := newTestProvider(t, nc)
	srv := matchingServer()
	srv.Labels = map[string]string{hcloudapi.LabelManagedBy: testCluster}
	servers.byID[1] = srv

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default"}},
		Status:     karpv1.NodeClaimStatus{ProviderID: "hcloud://1"},
	}
	got, err := cp.IsDrifted(context.Background(), claim)
	if err != nil || got != "" {
		t.Errorf("IsDrifted = %q, %v against a NodeClass whose own controller says its status is not trustworthy", got, err)
	}
}

// TestTransientReadFailureIsAnErrorNotDrift: a Hetzner blip must never be
// reported as a reason to replace a node.
func TestTransientReadFailureIsAnErrorNotDrift(t *testing.T) {
	nc := readyNodeClass()
	nc.Spec.Locations = []string{"nbg1"}
	cp, servers := newTestProvider(t, nc)
	servers.getErr = errTransient

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default"}},
		Status:     karpv1.NodeClaimStatus{ProviderID: "hcloud://1"},
	}
	got, err := cp.IsDrifted(context.Background(), claim)
	if err == nil {
		t.Fatalf("IsDrifted = %q, nil on a failed read; a blip must be retried, not acted on", got)
	}
	if got != "" {
		t.Errorf("IsDrifted returned reason %q alongside an error", got)
	}
}

// TestHashDriftIsReachableFromIsDrifted.
//
// A unit test of nodeClassHashDrift passes even when nothing calls it, which is
// exactly how this stayed dead: the annotation was never stamped on a NodeClaim,
// so the whole hash half of drift was unreachable in production while its unit
// tests stayed green.
func TestHashDriftIsReachableFromIsDrifted(t *testing.T) {
	nc := readyNodeClass()
	nc.Spec.Locations = []string{"nbg1"}
	nc.Annotations = map[string]string{
		v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion,
		v1alpha1.AnnotationHash:        "class-hash",
	}
	cp, servers := newTestProvider(t, nc)
	srv := matchingServer()
	srv.Labels = map[string]string{hcloudapi.LabelManagedBy: testCluster}
	servers.byID[1] = srv

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Annotations: map[string]string{
			v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion,
			v1alpha1.AnnotationHash:        "a-stale-hash",
		}},
		Spec:   karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default"}},
		Status: karpv1.NodeClaimStatus{ProviderID: "hcloud://1"},
	}
	got, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if got != NodeClassDrift {
		t.Errorf("IsDrifted = %q, want NodeClassDrift; the hash half of drift is not wired into IsDrifted", got)
	}
}

// TestEmptyStatusLocationsIsNotDrift.
//
// Defence in depth behind the readiness gate. Every validation failure path
// NILS status.locations, so if the gate is ever loosened this is the difference
// between "cannot judge" and "every node in the fleet is in the wrong place".
func TestEmptyStatusLocationsIsNotDrift(t *testing.T) {
	nc := resolvedNodeClass()
	nc.Spec.Locations = []string{"nbg1"} // the operator did write a list
	nc.Status.Locations = nil            // but nothing resolved

	if got := serverDrift(nc, matchingServer()); got != "" {
		t.Errorf("serverDrift = %q with no resolved locations at all; an empty set means unknown, not wrong", got)
	}
}

// TestClassWithoutAHashIsNotDrift: the hash controller has not reached the
// NodeClass yet. Absence is not evidence, in either direction.
func TestClassWithoutAHashIsNotDrift(t *testing.T) {
	nc := resolvedNodeClass()
	nc.Annotations = map[string]string{v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion}
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "n1",
		Annotations: map[string]string{
			v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion,
			v1alpha1.AnnotationHash:        "some-hash",
		},
	}}
	if got := nodeClassHashDrift(context.Background(), nc, claim); got != "" {
		t.Errorf("nodeClassHashDrift = %q with no hash on the NodeClass; that is a controller that has not caught up, not drift", got)
	}
}
