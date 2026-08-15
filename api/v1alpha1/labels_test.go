package v1alpha1

import (
	"testing"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// TestWellKnownLabelsRegistered checks the package init() ran.
//
// The consequence of it not running is subtle and severe: karpenter core's
// Requirements.Compatible() ALLOWS a well-known label that a NodePool template
// leaves undefined, but DENIES a custom one. Core's VolumeTopology injects a
// bound PV's nodeAffinity keys as NodeClaim requirements, so if
// csi.hetzner.cloud/location is not well-known, every pod with an existing
// hcloud volume becomes permanently unschedulable, with nothing in the
// scheduling error naming Karpenter as the cause.
func TestWellKnownLabelsRegistered(t *testing.T) {
	for _, key := range wellKnownLabels {
		if !karpv1.WellKnownLabels.Has(key) {
			t.Errorf("%s was not registered into karpenter WellKnownLabels", key)
		}
	}
}

// TestCSILocationIsWellKnown calls out the one key that is not ours but is
// load-bearing, so a future cleanup does not "tidy away" a foreign key.
func TestCSILocationIsWellKnown(t *testing.T) {
	if !karpv1.WellKnownLabels.Has(LabelCSILocation) {
		t.Fatalf("%s must be well-known: it is the Hetzner CSI topology key, and core injects "+
			"it as a NodeClaim requirement for any pod with a bound hcloud volume", LabelCSILocation)
	}
}

// TestSchedulingIsolationLabelsStayCustom is the inverse contract, and the
// reason wellKnownLabels is an explicit allow-list rather than a prefix sweep.
//
// A label used to ISOLATE workloads onto dedicated nodes must stay custom.
// Custom labels are denied when a NodePool template leaves them undefined,
// which is precisely what stops such pods scheduling onto general pools.
// Registering one as well-known inverts that to "allowed when undefined" and
// silently dissolves the isolation, with no error anywhere.
func TestSchedulingIsolationLabelsStayCustom(t *testing.T) {
	for _, key := range []string{"ci", "workload", "role", "dedicated"} {
		if karpv1.WellKnownLabels.Has(key) {
			t.Errorf("%q must NOT be well-known: registering an isolation label inverts "+
				"Compatible() from deny-when-undefined to allow-when-undefined and breaks "+
				"node isolation silently", key)
		}
	}
}
