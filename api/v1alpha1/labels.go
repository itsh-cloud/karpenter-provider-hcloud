package v1alpha1

import (
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Well-known label keys this provider sets on nodes and advertises on
// InstanceType and Offering requirements.
const (
	// LabelServerTypeLine is the Hetzner product line: cx, cpx, ccx, cax.
	// Lets a NodePool say `NotIn [cpx, ccx]` rather than enumerating SKUs.
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
	// nodes and onto every PV's nodeAffinity. It is NOT ours, but it must be
	// registered as well-known and carried on every Offering.
	//
	// Karpenter core's VolumeTopology reads a bound PV's nodeAffinity and
	// injects those keys as NodeClaim requirements. Requirements.Compatible()
	// denies *custom* labels that a NodePool template leaves undefined, while
	// allowing undefined *well-known* ones. So if this key is neither
	// registered nor present on our offerings, every pod with an existing
	// hcloud volume becomes permanently unschedulable, with nothing in the
	// error pointing at Karpenter.
	LabelCSILocation = "csi.hetzner.cloud/location"
)

// Values for LabelServerTypeLine.
const (
	ServerTypeLineCX  = "cx"
	ServerTypeLineCPX = "cpx"
	ServerTypeLineCCX = "ccx"
	ServerTypeLineCAX = "cax"
)

// wellKnownLabels are registered into karpenter core's WellKnownLabels set so
// that a NodePool template which leaves them undefined does not cause
// Compatible() to reject pods carrying them.
//
// Add a key here ONLY if undefined-means-allowed is the behaviour you want.
// The inverse matters just as much: a label used to steer workloads onto
// dedicated nodes must stay *custom*, so that leaving it undefined on the
// general NodePools DENIES those pods there. Registering such a label would
// silently dissolve the isolation.
//
// Note these describe intrinsic properties of the machine Hetzner sold you.
// Anything installed by the bootstrap is a property of the NodeClass, and is
// advertised with an ordinary NodePool template label rather than from here.
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
