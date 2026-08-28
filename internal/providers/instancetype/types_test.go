package instancetype

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// Catalog shaped like the real one: the three eu-central datacenters, and the
// prices recorded in the Hetzner June 2026 adjustment.
func testCatalog() []hcloudapi.ServerType {
	price := func(m float64) map[string]hcloudapi.Price {
		return map[string]hcloudapi.Price{
			"nbg1": {MonthlyNet: m}, "fsn1": {MonthlyNet: m}, "hel1": {MonthlyNet: m},
		}
	}
	sts := []hcloudapi.ServerType{
		{ID: 114, Name: "cx23", Cores: 2, MemoryGiB: 4, DiskGB: 40, Architecture: "x86", CPUType: "shared", StorageType: "local", Prices: price(5.49)},
		{ID: 115, Name: "cx33", Cores: 4, MemoryGiB: 8, DiskGB: 80, Architecture: "x86", CPUType: "shared", StorageType: "local", Prices: price(8.49)},
		{ID: 116, Name: "cx43", Cores: 8, MemoryGiB: 16, DiskGB: 160, Architecture: "x86", CPUType: "shared", StorageType: "local", Prices: price(15.99)},
		{ID: 110, Name: "cpx32", Cores: 4, MemoryGiB: 8, DiskGB: 160, Architecture: "x86", CPUType: "shared", StorageType: "local", Prices: price(35.49)},
		{ID: 45, Name: "cax11", Cores: 2, MemoryGiB: 4, DiskGB: 40, Architecture: "arm", CPUType: "shared", StorageType: "local", Prices: price(4.49)},
		{ID: 999, Name: "cx11", Cores: 1, MemoryGiB: 2, DiskGB: 20, Architecture: "x86", Deprecated: true, Prices: price(3.29)},
	}
	// Mirrors the live API on 2026-08-16: cx43 reported unavailable on the
	// mainland, available in Helsinki. Ordering it in nbg1 succeeded anyway.
	locs := func(availMainland bool) []hcloudapi.ServerTypeLocation {
		return []hcloudapi.ServerTypeLocation{
			{Location: "nbg1", NetworkZone: "eu-central", Available: availMainland},
			{Location: "fsn1", NetworkZone: "eu-central", Available: availMainland},
			{Location: "hel1", NetworkZone: "eu-central", Available: true},
		}
	}
	for i := range sts {
		sts[i].Locations = locs(sts[i].Name == "cx23" || sts[i].Name == "cx33")
	}
	return sts
}

func testNodeClass(locations ...string) *v1alpha1.HCloudNodeClass {
	return &v1alpha1.HCloudNodeClass{
		Spec: v1alpha1.HCloudNodeClassSpec{Locations: locations},
	}
}

// TestEveryOfferingCarriesCSILocation.
//
// Core injects a bound PV's nodeAffinity keys as NodeClaim requirements and
// Requirements.Compatible() DENIES a custom label a NodePool template leaves
// undefined. Every hcloud PV carries nodeAffinity on csi.hetzner.cloud/location
// and nothing else, so an offering omitting this key makes every pod with an
// existing volume permanently unschedulable, with nothing in the scheduling
// error pointing at Karpenter.
func TestEveryOfferingCarriesCSILocation(t *testing.T) {
	sts := testCatalog()
	its := Build(sts, testNodeClass(), 0, NewUnavailable(), Options{})

	if len(its) == 0 {
		t.Fatal("no instance types built")
	}
	for _, it := range its {
		for _, o := range it.Offerings {
			got := o.Requirements.Get(v1alpha1.LabelCSILocation)
			if got.Len() != 1 {
				t.Errorf("%s offering missing %s", it.Name, v1alpha1.LabelCSILocation)
				continue
			}
			// It must be the LOCATION (nbg1), not the datacenter (nbg1-dc3):
			// that is what the CSI driver writes onto PVs and nodes.
			region := o.Requirements.Get(corev1.LabelTopologyRegion)
			if got.Any() != region.Any() {
				t.Errorf("%s: csi location %q != region %q", it.Name, got.Any(), region.Any())
			}
		}
		// And on the instance type itself, or a pod whose requirement is
		// pinned to a location cannot match the type at all.
		if it.Requirements.Get(v1alpha1.LabelCSILocation).Len() == 0 {
			t.Errorf("%s instance type missing %s", it.Name, v1alpha1.LabelCSILocation)
		}
	}
}

// TestOfferingsCarryTheZoneKarpenterWillPriceAgainst.
//
// Karpenter prices a running node with OfferingPrice(node zone label, capacity
// type). If no offering carries that zone the candidate prices at 0 and
// consolidation looks for replacements cheaper than 0, finding none, so
// replacement consolidation silently stops working. Leaving zone unconstrained
// does not avoid that, see LegacyDatacenterForLocation. This asserts the
// end-to-end property, the price lookup succeeding, not just a label.
func TestOfferingsCarryTheZoneKarpenterWillPriceAgainst(t *testing.T) {
	its := Build(testCatalog(), testNodeClass("nbg1"), 0, NewUnavailable(), Options{})
	if len(its) == 0 {
		t.Fatal("no instance types built")
	}

	for _, it := range its {
		for _, o := range it.Offerings {
			if got := o.Requirements.Get(corev1.LabelTopologyRegion).Any(); got != "nbg1" {
				t.Errorf("%s region = %q, want the location nbg1", it.Name, got)
			}
		}

		// The label hcloud-CCM will actually write on a node in nbg1.
		price, ok := it.OfferingPrice("nbg1-dc3", karpv1.CapacityTypeOnDemand)
		if !ok {
			t.Errorf("%s: OfferingPrice(nbg1-dc3) found nothing; consolidation would price this node at 0 "+
				"and never replace it", it.Name)
			continue
		}
		if price <= 0 {
			t.Errorf("%s: OfferingPrice(nbg1-dc3) = %v, want a positive hourly price", it.Name, price)
		}
	}
}

// TestZoneMatchesTheCCMMapping pins the values themselves against
// hcloud-cloud-controller-manager's internal/legacydatacenter.NameFromLocation,
// which is the function that actually writes the label. A divergence here is
// invisible: nodes simply stop being consolidatable.
func TestZoneMatchesTheCCMMapping(t *testing.T) {
	for location, want := range map[string]string{
		"nbg1": "nbg1-dc3",
		"hel1": "hel1-dc2",
		"fsn1": "fsn1-dc14",
		"ash":  "ash-dc1",
		"hil":  "hil-dc1",
		"sin":  "sin-dc1",
		// The CCM's own default: a location it does not know maps to itself,
		// which is what it promises for locations added later.
		"xyz1": "xyz1",
	} {
		if got := LegacyDatacenterForLocation(location); got != want {
			t.Errorf("LegacyDatacenterForLocation(%q) = %q, want %q to match what the CCM writes", location, got, want)
		}
	}
}

// TestAvailabilityIgnoresHetznerFlag.
//
// The fixture reports cx43 unavailable in nbg1 and fsn1, mirroring the live
// API. Ordering such a type has been observed to succeed, so the offering must
// still be Available: gating on the flag would exclude precisely the cheap type
// worth returning to after a stockout, keeping an expensive fallback alive.
func TestAvailabilityIgnoresHetznerFlag(t *testing.T) {
	sts := testCatalog()
	its := Build(sts, testNodeClass("nbg1"), 0, NewUnavailable(), Options{})

	for _, it := range its {
		if it.Name != "cx43" {
			continue
		}
		for _, o := range it.Offerings {
			if !o.Available {
				t.Error("cx43 in nbg1 is not Available, but Hetzner's flag must not gate " +
					"provisioning: it is neither sufficient nor necessary")
			}
		}
		return
	}
	t.Fatal("cx43 missing from the catalog")
}

// TestObservedFailureSuppressesOffering: the signal that DOES count.
func TestObservedFailureSuppressesOffering(t *testing.T) {
	sts := testCatalog()
	u := NewUnavailable()
	u.Mark("cx43", "nbg1", "resource_unavailable")

	its := Build(sts, testNodeClass(), 0, u, Options{})

	for _, it := range its {
		if it.Name != "cx43" {
			continue
		}
		for _, o := range it.Offerings {
			region := o.Requirements.Get(corev1.LabelTopologyRegion).Any()
			if region == "nbg1" && o.Available {
				t.Error("an observed capacity failure did not suppress the offering")
			}
			// Suppression is per location: a stockout in Nuremberg says nothing
			// about Helsinki, and over-suppressing strands capacity.
			if region == "hel1" && !o.Available {
				t.Error("suppression leaked into another location")
			}
		}
	}
}

// TestPriceUsesMonthlyCap: the monthly figure is a cap, not 730x hourly.
func TestPriceUsesMonthlyCap(t *testing.T) {
	sts := testCatalog()
	its := Build(sts, testNodeClass("nbg1"), 0, NewUnavailable(), Options{})

	want := map[string]float64{"cx23": 5.49, "cx33": 8.49, "cx43": 15.99, "cpx32": 35.49}
	for _, it := range its {
		w, ok := want[it.Name]
		if !ok {
			continue
		}
		got := it.Offerings[0].Price
		if diff := got - w/HoursPerMonth; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s price = %v, want %v", it.Name, got, w/HoursPerMonth)
		}
	}
}

// TestPriceOrderingMatchesTheExpander: the ordering the hand-maintained
// priority expander encoded must fall out of price alone within a pool.
func TestPriceOrderingMatchesTheExpander(t *testing.T) {
	sts := testCatalog()
	its := Build(sts, testNodeClass("nbg1"), 0, NewUnavailable(), Options{})

	price := map[string]float64{}
	for _, it := range its {
		price[it.Name] = it.Offerings[0].Price
	}

	// Absolute price, which is what Karpenter sorts by. Note this is NOT the
	// EUR-per-usable-GB ordering: cx33 is cheaper than cx43 absolutely while
	// being worse per usable GB, which is exactly why that judgement has to be
	// expressed as NodePool membership rather than left to price.
	if !(price["cx23"] < price["cx33"] && price["cx33"] < price["cx43"] && price["cx43"] < price["cpx32"]) {
		t.Errorf("unexpected price ordering: %v", price)
	}
}

// TestPublicIPv4SurchargeIncluded: a primary IPv4 is billed separately, so
// omitting it understates every offering that has one.
func TestPublicIPv4SurchargeIncluded(t *testing.T) {
	sts := testCatalog()
	nc := testNodeClass("nbg1")

	withIP := Build(sts, nc, 0.60, NewUnavailable(), Options{})

	no := false
	nc.Spec.PublicIPv4 = &no
	withoutIP := Build(sts, nc, 0.60, NewUnavailable(), Options{})

	delta := withIP[0].Offerings[0].Price - withoutIP[0].Offerings[0].Price
	if want := 0.60 / HoursPerMonth; delta < want-1e-9 || delta > want+1e-9 {
		t.Errorf("IPv4 surcharge = %v, want %v", delta, want)
	}
}

// TestArchitectureAndDeprecationFilters: arm and deprecated types are excluded
// from a provider scoped to x86.
func TestFilters(t *testing.T) {
	sts := testCatalog()
	its := Build(sts, testNodeClass(), 0, NewUnavailable(), Options{})

	for _, it := range its {
		if it.Name == "cax11" {
			t.Error("arm type included in an x86-only catalog")
		}
		if it.Name == "cx11" {
			t.Error("deprecated type included")
		}
		if got := it.Requirements.Get(corev1.LabelArchStable).Any(); got != karpv1.ArchitectureAmd64 {
			t.Errorf("%s arch = %q, want amd64", it.Name, got)
		}
	}
}

// TestLocationScoping: spec.locations bounds which datacenters appear.
func TestLocationScoping(t *testing.T) {
	sts := testCatalog()
	its := Build(sts, testNodeClass("nbg1", "fsn1"), 0, NewUnavailable(), Options{})

	for _, it := range its {
		for _, o := range it.Offerings {
			if got := o.Requirements.Get(corev1.LabelTopologyRegion).Any(); got == "hel1" {
				t.Errorf("%s offered in hel1, which is outside spec.locations", it.Name)
			}
		}
	}
}

// TestDerivedLabels: the shape labels a NodePool uses to express intent
// without enumerating SKUs.
func TestDerivedLabels(t *testing.T) {
	sts := testCatalog()
	its := Build(sts, testNodeClass("nbg1"), 0, NewUnavailable(), Options{})

	for _, it := range its {
		r := it.Requirements
		switch it.Name {
		case "cx43":
			if got := r.Get(v1alpha1.LabelServerTypeLine).Any(); got != "cx" {
				t.Errorf("cx43 line = %q, want cx", got)
			}
			if got := r.Get(v1alpha1.LabelCPUVendor).Any(); got != "intel" {
				t.Errorf("cx43 vendor = %q, want intel", got)
			}
			if got := r.Get(v1alpha1.LabelVCPU).Any(); got != "8" {
				t.Errorf("cx43 vcpu = %q, want 8", got)
			}
		case "cpx32":
			if got := r.Get(v1alpha1.LabelServerTypeLine).Any(); got != "cpx" {
				t.Errorf("cpx32 line = %q, want cpx", got)
			}
			if got := r.Get(v1alpha1.LabelCPUVendor).Any(); got != "amd" {
				t.Errorf("cpx32 vendor = %q, want amd", got)
			}
		}
	}
}

// TestMissingPriceDropsOffering: a location with no price does not sell the
// type. It must not become a zero-priced offering, which would win every
// comparison Karpenter makes and pull the whole fleet onto it.
func TestMissingPriceDropsOffering(t *testing.T) {
	sts := testCatalog()
	for i := range sts {
		if sts[i].Name == "cx43" {
			sts[i].Prices = map[string]hcloudapi.Price{"hel1": {MonthlyNet: 15.99}}
		}
	}

	its := Build(sts, testNodeClass(), 0, NewUnavailable(), Options{})
	for _, it := range its {
		if it.Name != "cx43" {
			continue
		}
		for _, o := range it.Offerings {
			if o.Price == 0 {
				t.Fatal("an unpriced location produced a zero-cost offering")
			}
			if got := o.Requirements.Get(corev1.LabelTopologyRegion).Any(); got != "hel1" {
				t.Errorf("cx43 offered in %q, which has no price", got)
			}
		}
	}
}
