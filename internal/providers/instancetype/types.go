package instancetype

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// HoursPerMonth is Hetzner's billing month, used to convert the monthly cap to
// the hourly figure Karpenter compares offerings by.
//
// The monthly price is a CAP rather than 730x the hourly rate, and the ratio
// differs per SKU. Karpenter nodes are long-lived, so the capped monthly figure
// is the honest basis; deriving from the hourly rate would systematically
// over-price the larger types and distort consolidation decisions.
const HoursPerMonth = 730

// Options tunes catalog construction.
type Options struct {
	MemoryOverheadPercent float64
	DiskOverheadPercent   float64
	// IncludeArchitectures limits which architectures are offered. Empty means
	// x86 only, matching this provider's supported scope.
	IncludeArchitectures []string
}

func (o Options) withDefaults() Options {
	if o.MemoryOverheadPercent == 0 {
		o.MemoryOverheadPercent = DefaultVMMemoryOverheadPercent
	}
	if o.DiskOverheadPercent == 0 {
		o.DiskOverheadPercent = DefaultVMDiskOverheadPercent
	}
	if len(o.IncludeArchitectures) == 0 {
		o.IncludeArchitectures = []string{"x86"}
	}
	return o
}

// Build turns the Hetzner catalog into Karpenter instance types for one
// NodeClass.
//
// locations optionally restricts which Hetzner locations are in scope; empty
// means every location the datacenters list reports.
func Build(
	serverTypes []hcloudapi.ServerType,
	nodeClass *v1alpha1.HCloudNodeClass,
	primaryIPv4Monthly float64,
	unavailable *Unavailable,
	opts Options,
) []*cloudprovider.InstanceType {
	opts = opts.withDefaults()

	inScope := locationFilter(nodeClass.Spec.Locations)
	arches := map[string]bool{}
	for _, a := range opts.IncludeArchitectures {
		arches[a] = true
	}

	var out []*cloudprovider.InstanceType
	for _, st := range serverTypes {
		if st.Deprecated || !arches[st.Architecture] {
			continue
		}
		locs := make([]hcloudapi.ServerTypeLocation, 0, len(st.Locations))
		for _, l := range st.Locations {
			// A location-scoped deprecation is a phase-out of this type there,
			// so treat it as not offered rather than provisioning onto a type
			// that is going away underneath us.
			if !inScope(l.Location) || l.Deprecated {
				continue
			}
			locs = append(locs, l)
		}
		if len(locs) == 0 {
			continue
		}
		if it := newInstanceType(st, locs, nodeClass, primaryIPv4Monthly, unavailable, opts); it != nil {
			out = append(out, it)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func newInstanceType(
	st hcloudapi.ServerType,
	locs []hcloudapi.ServerTypeLocation,
	nodeClass *v1alpha1.HCloudNodeClass,
	primaryIPv4Monthly float64,
	unavailable *Unavailable,
	opts Options,
) *cloudprovider.InstanceType {
	maxPods := nodeClass.Spec.Kubelet.MaxPodsOrDefault()
	capacity := Capacity(st.Cores, st.MemoryGiB, st.DiskGB, maxPods,
		opts.MemoryOverheadPercent, opts.DiskOverheadPercent)

	offerings := make(cloudprovider.Offerings, 0, len(locs))
	regions := map[string]bool{}

	for _, l := range locs {
		price, ok := st.Prices[l.Location]
		if !ok {
			// No price for this location means the type is not sold there.
			// A missing price must never become a zero one: a free-looking
			// offering would win every comparison Karpenter makes.
			continue
		}

		monthly := price.MonthlyNet
		if nodeClass.Spec.PublicIPv4Enabled() {
			monthly += primaryIPv4Monthly
		}

		offerings = append(offerings, &cloudprovider.Offering{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, l.Location),
				// Zone, and it must be here: Karpenter prices a running node by
				// matching an offering against its topology.kubernetes.io/zone
				// label, and an offering carrying no zone can never match, so
				// every candidate prices at 0 and consolidation silently stops
				// finding replacements. The value is derived, not guessed, see
				// LegacyDatacenterForLocation.
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn,
					LegacyDatacenterForLocation(l.Location)),
				// Load-bearing. Core injects a bound PV's nodeAffinity keys as
				// NodeClaim requirements and Compatible() DENIES a custom label
				// a NodePool leaves undefined, so without this every pod with an
				// existing hcloud volume is permanently unschedulable and
				// nothing names Karpenter as the cause.
				scheduling.NewRequirement(v1alpha1.LabelCSILocation, corev1.NodeSelectorOpIn, l.Location),
				scheduling.NewRequirement(v1alpha1.LabelNetworkZone, corev1.NodeSelectorOpIn, l.NetworkZone),
			),
			Price: monthly / HoursPerMonth,
			// Driven solely by observed failures, never by Hetzner's published
			// availability flag. See Unavailable.
			Available: !unavailable.Has(st.Name, l.Location),
		})

		regions[l.Location] = true
	}

	if len(offerings) == 0 {
		return nil
	}

	return &cloudprovider.InstanceType{
		Name:         st.Name,
		Capacity:     capacity,
		Overhead:     Overhead(&nodeClass.Spec.Kubelet, capacity),
		Offerings:    offerings,
		Requirements: instanceTypeRequirements(st, keys(regions)),
	}
}

func instanceTypeRequirements(st hcloudapi.ServerType, regions []string) scheduling.Requirements {
	arch := karpv1.ArchitectureAmd64
	if st.Architecture == "arm" {
		arch = karpv1.ArchitectureArm64
	}

	return scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, st.Name),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, arch),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, regions...),
		scheduling.NewRequirement(v1alpha1.LabelCSILocation, corev1.NodeSelectorOpIn, regions...),

		// Derived shape, so a NodePool can express intent without enumerating
		// SKUs: "NotIn [cpx, ccx]" rather than listing every type in a line.
		scheduling.NewRequirement(v1alpha1.LabelServerTypeLine, corev1.NodeSelectorOpIn, st.Line()),
		scheduling.NewRequirement(v1alpha1.LabelCPUType, corev1.NodeSelectorOpIn, st.CPUType),
		scheduling.NewRequirement(v1alpha1.LabelCPUVendor, corev1.NodeSelectorOpIn, cpuVendor(st.Line())),
		scheduling.NewRequirement(v1alpha1.LabelStorageType, corev1.NodeSelectorOpIn, st.StorageType),
		scheduling.NewRequirement(v1alpha1.LabelVCPU, corev1.NodeSelectorOpIn, fmt.Sprint(st.Cores)),
		scheduling.NewRequirement(v1alpha1.LabelMemoryGB, corev1.NodeSelectorOpIn, fmt.Sprintf("%g", st.MemoryGiB)),
		scheduling.NewRequirement(v1alpha1.LabelDiskGB, corev1.NodeSelectorOpIn, fmt.Sprint(st.DiskGB)),
	)
}

// cpuVendor maps a product line to its silicon. CX is Intel, CPX and CCX are
// AMD, CAX is Ampere.
func cpuVendor(line string) string {
	switch line {
	case v1alpha1.ServerTypeLineCX:
		return "intel"
	case v1alpha1.ServerTypeLineCPX, v1alpha1.ServerTypeLineCCX:
		return "amd"
	case v1alpha1.ServerTypeLineCAX:
		return "ampere"
	default:
		return "unknown"
	}
}

func locationFilter(allowed []string) func(string) bool {
	if len(allowed) == 0 {
		return func(string) bool { return true }
	}
	set := make(map[string]bool, len(allowed))
	for _, l := range allowed {
		set[l] = true
	}
	return func(l string) bool { return set[l] }
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
