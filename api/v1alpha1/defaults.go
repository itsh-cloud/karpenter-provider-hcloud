package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Defaults mirroring the kubebuilder markers on these fields.
//
// The duplication is deliberate: CRD defaulting only descends into a struct
// that is PRESENT in the submitted object, so a manifest omitting
// `spec.bootstrap.containerd` entirely never has its apt pin applied. The Go
// accessors below are the source of truth; the markers document them.
const (
	DefaultContainerdAptPin = "2.*"
	DefaultPackageRevision  = "1.1"
	DefaultMaxPods          = int32(110)
)

// DefaultKubeReserved and friends reproduce the reservations a node actually
// runs with. Karpenter subtracts these when computing allocatable, so if they
// disagree with what the kubelet is given it overpacks every node by the
// difference and pods sit Pending on nodes it believes have room.
func DefaultKubeReserved() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("200m"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	}
}

func DefaultSystemReserved() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("200m"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	}
}

// DefaultEvictionHard is kubelet's full default signal set.
//
// --eviction-hard REPLACES this map rather than merging into it, so a partial
// override silently disables every signal it omits.
func DefaultEvictionHard() map[string]string {
	return map[string]string{
		"memory.available":   "500Mi",
		"nodefs.available":   "10%",
		"nodefs.inodesFree":  "5%",
		"imagefs.available":  "15%",
		"imagefs.inodesFree": "5%",
	}
}

// SystemdCgroupEnabled reports whether containerd uses the systemd cgroup
// driver, which must match the kubelet's.
func (in *ContainerdSpec) SystemdCgroupEnabled() bool {
	if in == nil || in.SystemdCgroup == nil {
		return true
	}
	return *in.SystemdCgroup
}

// AptPinOrDefault returns the containerd.io apt version constraint, holding
// containerd within a major. See ContainerdSpec.AptPin for why.
func (in *ContainerdSpec) AptPinOrDefault() string {
	if in == nil || in.AptPin == "" {
		return DefaultContainerdAptPin
	}
	return in.AptPin
}

// UnattendedUpgradesEnabled reports whether to configure unattended-upgrades.
func (in *UnattendedUpgradesSpec) UnattendedUpgradesEnabled() bool {
	if in == nil || in.Enabled == nil {
		return true
	}
	return *in.Enabled
}

// RemoveUnusedKernelsEnabled reports whether superseded kernels are purged,
// which otherwise fill /boot.
func (in *UnattendedUpgradesSpec) RemoveUnusedKernelsEnabled() bool {
	if in == nil || in.RemoveUnusedKernelPackages == nil {
		return true
	}
	return *in.RemoveUnusedKernelPackages
}

// PackageRevisionOrDefault returns the Debian package revision suffix.
func (in *BootstrapSpec) PackageRevisionOrDefault() string {
	if in == nil || in.PackageRevision == "" {
		return DefaultPackageRevision
	}
	return in.PackageRevision
}

// PackageUpgradeOnBootEnabled reports whether cloud-init runs a full package
// upgrade. See BootstrapSpec.PackageUpgradeOnBoot for the trade it makes.
func (in *BootstrapSpec) PackageUpgradeOnBootEnabled() bool {
	if in == nil || in.PackageUpgradeOnBoot == nil {
		return true
	}
	return *in.PackageUpgradeOnBoot
}

// KubeReservedOrDefault returns the kube-reserved resources.
func (in *KubeletConfiguration) KubeReservedOrDefault() corev1.ResourceList {
	if in == nil || len(in.KubeReserved) == 0 {
		return DefaultKubeReserved()
	}
	return in.KubeReserved
}

// SystemReservedOrDefault returns the system-reserved resources.
func (in *KubeletConfiguration) SystemReservedOrDefault() corev1.ResourceList {
	if in == nil || len(in.SystemReserved) == 0 {
		return DefaultSystemReserved()
	}
	return in.SystemReserved
}

// EvictionHardOrDefault returns the hard eviction thresholds.
func (in *KubeletConfiguration) EvictionHardOrDefault() map[string]string {
	if in == nil || len(in.EvictionHard) == 0 {
		return DefaultEvictionHard()
	}
	return in.EvictionHard
}

// MaxPodsOrDefault returns the pod cap, which is also the "pods" resource
// Karpenter bin-packs against.
func (in *KubeletConfiguration) MaxPodsOrDefault() int32 {
	if in == nil || in.MaxPods == nil {
		return DefaultMaxPods
	}
	return *in.MaxPods
}

// PublicIPv4Enabled reports whether nodes get a primary IPv4, which is billed
// separately and is included in the offering price when true.
func (in *HCloudNodeClassSpec) PublicIPv4Enabled() bool {
	if in == nil || in.PublicIPv4 == nil {
		return true
	}
	return *in.PublicIPv4
}

// PublicIPv6Enabled reports whether nodes get a primary IPv6.
func (in *HCloudNodeClassSpec) PublicIPv6Enabled() bool {
	if in == nil || in.PublicIPv6 == nil {
		return true
	}
	return *in.PublicIPv6
}

// ImageDriftPolicyOrDefault returns the image drift policy, defaulting to
// Ignore.
//
// Ignore is the safe default: Hetzner rebuilds its named images every few
// weeks, so a name maps to a new id on Hetzner's schedule, and Replace would
// roll the entire fleet whenever they did, an outage the operator neither
// asked for nor could predict.
func (in *HCloudNodeClassSpec) ImageDriftPolicyOrDefault() ImageDriftPolicy {
	if in == nil || in.ImageDriftPolicy == "" {
		return ImageDriftPolicyIgnore
	}
	return in.ImageDriftPolicy
}
