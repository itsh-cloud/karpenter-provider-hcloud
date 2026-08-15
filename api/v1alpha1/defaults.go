package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Defaults matching the values these fields carry in their kubebuilder
// markers. They are duplicated here deliberately, and the reason is not
// belt-and-braces.
//
// CRD defaulting only descends into a struct that is PRESENT in the submitted
// object. A manifest that omits `spec.bootstrap.containerd` entirely never has
// its apt pin applied, so the field stays empty rather than becoming
// true. That is precisely the silent omission the default was chosen to make
// impossible, so the Go accessors below are the real source of truth and the
// markers are the documentation of them.
const (
	DefaultContainerdAptPin = "2.*"
	DefaultPackageRevision  = "1.1"
	DefaultMaxPods          = int32(110)
)

// DefaultKubeReserved and friends reproduce the reservations a node actually
// runs with. Karpenter subtracts these when computing allocatable, so if they
// ever disagree with what the kubelet is given, Karpenter overpacks every node
// by the difference and pods sit Pending on nodes it believes have room.
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

// AptPinOrDefault returns the containerd.io apt version constraint. It holds
// containerd within a major: unattended upgrades must patch it, but crossing a
// major changes the CRI plugin configuration shape and breaks any additional
// runtime handler wired into it.
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
// upgrade. It adds one to four minutes to every boot against core's hardcoded
// 15-minute registration timeout.
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
