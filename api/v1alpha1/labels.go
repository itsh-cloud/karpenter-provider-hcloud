package v1alpha1

import (
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Well-known label keys this provider sets on nodes and advertises on
// InstanceType and Offering requirements.
const (
	// LabelServerTypeLine is the Hetzner product line: cx, cpx, ccx, cax. Lets
	// a NodePool say `NotIn [cpx, ccx]` rather than enumerating SKUs.
	LabelServerTypeLine = Group + "/server-type-line"

	// LabelCPUType is "shared" or "dedicated", from ServerType.CPUType.
	LabelCPUType = Group + "/cpu-type"

	// LabelCPUVendor is "intel", "amd" or "ampere", derived from the line.
	LabelCPUVendor = Group + "/cpu-vendor"

	// LabelStorageType is "local" or "network", from ServerType.StorageType.
	LabelStorageType = Group + "/storage-type"

	// LabelVCPU, LabelMemoryGB and LabelDiskGB carry the numeric shape of the
	// server type so NodePools can express Gt/Lt requirements.
	LabelVCPU     = Group + "/vcpu"
	LabelMemoryGB = Group + "/memory-gb"
	LabelDiskGB   = Group + "/disk-gb"

	// LabelNetworkZone is the Hetzner network zone, e.g. eu-central. Servers
	// in one zone share a private network.
	LabelNetworkZone = Group + "/network-zone"

	// LabelCSILocation is the topology key the Hetzner CSI driver writes onto
	// nodes and onto every PV's nodeAffinity. Not ours, but it must be
	// registered well-known and carried on every Offering: core's
	// VolumeTopology injects a bound PV's nodeAffinity keys as NodeClaim
	// requirements, and Requirements.Compatible() denies *custom* labels a
	// NodePool template leaves undefined while allowing undefined *well-known*
	// ones. Without this, every pod with an existing hcloud volume becomes
	// permanently unschedulable, with nothing in the error naming Karpenter.
	LabelCSILocation = "csi.hetzner.cloud/location"
)

// Values for LabelServerTypeLine.
const (
	ServerTypeLineCX  = "cx"
	ServerTypeLineCPX = "cpx"
	ServerTypeLineCCX = "ccx"
	ServerTypeLineCAX = "cax"
)

// wellKnownLabels are registered into karpenter core's WellKnownLabels so that
// a NodePool template leaving them undefined does not make Compatible() reject
// pods carrying them.
//
// Add a key here ONLY if undefined-means-allowed is what you want. A label used
// to steer workloads onto dedicated nodes must stay *custom*, because leaving
// it undefined on the general NodePools is what DENIES those pods there.
// These keys are intrinsic properties of the machine; anything the bootstrap
// installs is advertised with an ordinary NodePool template label instead.
var wellKnownLabels = []string{
	LabelServerTypeLine,
	LabelCPUType,
	LabelCPUVendor,
	LabelStorageType,
	LabelVCPU,
	LabelMemoryGB,
	LabelDiskGB,
	LabelNetworkZone,
	LabelCSILocation,
}

func init() {
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Insert(wellKnownLabels...)
}
