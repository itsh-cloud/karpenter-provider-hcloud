package v1alpha1

import (
	"testing"

	"github.com/samber/lo"
)

// TestAccessorsOnAbsentBlocks is the regression test for the reason
// defaults.go exists.
//
// CRD defaulting only descends into structs PRESENT in the submitted object.
// A manifest that omits `spec.bootstrap.gvisor` never gets
// `gvisor.enabled: true` applied, so the field arrives nil. Verified against a
// live API server with a manifest omitting those blocks: `containerd`,
// `gvisor` and `unattendedUpgrades` were entirely absent from the returned
// object, while sibling scalars at a present level (osFamily, mode,
// packageRevision, packageUpgradeOnBoot) were all defaulted.
//
// So a zero-valued spec must still yield the safe answers, or gVisor silently
// does not get installed and tenant pods fail at container-create on a node
// that looks healthy.
func TestAccessorsOnAbsentBlocks(t *testing.T) {
	var spec HCloudNodeClassSpec // everything nil or zero

	if !spec.Bootstrap.GVisor.GVisorEnabled() {
		t.Error("gVisor must default ON when the block is absent; a node without runsc " +
			"breaks tenant pods at container-create, not at scheduling")
	}
	if got := spec.Bootstrap.GVisor.NetworkOrDefault(); got != DefaultGVisorNetwork {
		t.Errorf("gVisor network = %q, want %q (netstack is incompatible with Cilium kube-proxy replacement)", got, DefaultGVisorNetwork)
	}
	if got := spec.Bootstrap.GVisor.RuntimeHandlerOrDefault(); got != DefaultGVisorHandler {
		t.Errorf("runtime handler = %q, want %q", got, DefaultGVisorHandler)
	}
	if got := spec.Bootstrap.GVisor.NodeLabelOrDefault(); got != LabelGVisor {
		t.Errorf("node label = %q, want %q", got, LabelGVisor)
	}
	if !spec.Bootstrap.Containerd.SystemdCgroupEnabled() {
		t.Error("systemd cgroup must default on; it has to match the kubelet's driver")
	}
	if got := spec.Bootstrap.Containerd.AptPinOrDefault(); got != DefaultContainerdAptPin {
		t.Errorf("apt pin = %q, want %q (crossing a containerd major breaks the CRI config shape)", got, DefaultContainerdAptPin)
	}
	if !spec.Bootstrap.UnattendedUpgrades.UnattendedUpgradesEnabled() {
		t.Error("unattended-upgrades must default on")
	}
	if !spec.Bootstrap.PackageUpgradeOnBootEnabled() {
		t.Error("packageUpgradeOnBoot must default on")
	}
	if got := spec.Bootstrap.PackageRevisionOrDefault(); got != DefaultPackageRevision {
		t.Errorf("package revision = %q, want %q", got, DefaultPackageRevision)
	}
	if !spec.PublicIPv4Enabled() || !spec.PublicIPv6Enabled() {
		t.Error("public IPs must default on, matching the CRD markers")
	}
}

// TestExplicitFalseIsHonoured: the nil-means-true accessors must not swallow a
// deliberate false.
func TestExplicitFalseIsHonoured(t *testing.T) {
	spec := HCloudNodeClassSpec{
		PublicIPv4: lo.ToPtr(false),
		Bootstrap: BootstrapSpec{
			PackageUpgradeOnBoot: lo.ToPtr(false),
			GVisor:               GVisorSpec{Enabled: lo.ToPtr(false)},
			Containerd:           ContainerdSpec{SystemdCgroup: lo.ToPtr(false)},
			UnattendedUpgrades:   UnattendedUpgradesSpec{Enabled: lo.ToPtr(false)},
		},
	}

	if spec.PublicIPv4Enabled() {
		t.Error("explicit publicIPv4=false ignored")
	}
	if spec.Bootstrap.PackageUpgradeOnBootEnabled() {
		t.Error("explicit packageUpgradeOnBoot=false ignored")
	}
	if spec.Bootstrap.GVisor.GVisorEnabled() {
		t.Error("explicit gvisor.enabled=false ignored")
	}
	if spec.Bootstrap.Containerd.SystemdCgroupEnabled() {
		t.Error("explicit systemdCgroup=false ignored")
	}
	if spec.Bootstrap.UnattendedUpgrades.UnattendedUpgradesEnabled() {
		t.Error("explicit unattendedUpgrades.enabled=false ignored")
	}
}

// TestKubeletDefaultsMatchOverheadModel pins the reservation totals.
//
// These exact numbers are what Karpenter subtracts to compute allocatable.
// Measured on live workers: capacity - allocatable is exactly 1524Mi of memory
// (512 kube + 512 system + 500 eviction) and 400m of CPU (200 + 200). If these
// drift from what the kubelet is actually given, Karpenter overpacks every
// node by the difference and there is no alert for it.
func TestKubeletDefaultsMatchOverheadModel(t *testing.T) {
	var k KubeletConfiguration

	kube, system := k.KubeReservedOrDefault(), k.SystemReservedOrDefault()

	cpu := kube.Cpu().MilliValue() + system.Cpu().MilliValue()
	if cpu != 400 {
		t.Errorf("reserved CPU = %dm, want 400m", cpu)
	}

	memMi := (kube.Memory().Value() + system.Memory().Value()) / (1024 * 1024)
	if memMi != 1024 {
		t.Errorf("reserved memory = %dMi, want 1024Mi", memMi)
	}

	// --eviction-hard replaces kubelet's whole default map, so a partial
	// override silently disables every signal it omits.
	evict := k.EvictionHardOrDefault()
	for _, signal := range []string{
		"memory.available", "nodefs.available", "nodefs.inodesFree",
		"imagefs.available", "imagefs.inodesFree",
	} {
		if _, ok := evict[signal]; !ok {
			t.Errorf("default evictionHard is missing %q; --eviction-hard replaces "+
				"the map wholesale, so omitting a signal disables it", signal)
		}
	}
	if got := evict["memory.available"]; got != "500Mi" {
		t.Errorf("memory.available = %q, want 500Mi (part of the measured 1524Mi overhead)", got)
	}
}
