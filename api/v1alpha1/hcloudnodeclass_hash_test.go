package v1alpha1

import (
	"testing"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fixture returns a fully-populated NodeClass. Both hashed and ignored fields
// are set, so a test that mutates one can tell the two apart.
func fixture() *HCloudNodeClass {
	return &HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "worker"},
		Spec: HCloudNodeClassSpec{
			ImageSelector:     ImageSelectorTerm{Name: "debian-13"},
			ImageDriftPolicy:  ImageDriftPolicyIgnore,
			Locations:         []string{"nbg1", "fsn1"},
			NetworkSelector:   &NetworkSelectorTerm{Name: "k8s-network"},
			FirewallSelectors: []FirewallSelectorTerm{{Name: "k8s-fw"}},
			SSHKeySelectors:   []SSHKeySelectorTerm{{Name: "ops@example"}},
			PublicIPv4:        lo.ToPtr(true),
			PublicIPv6:        lo.ToPtr(true),
			ServerLabels:      map[string]string{"cost-center": "platform"},
			Kubelet: KubeletConfiguration{
				KubeReserved: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				SystemReserved: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				EvictionHard: map[string]string{"memory.available": "500Mi"},
				MaxPods:      lo.ToPtr(int32(110)),
			},
			Bootstrap: BootstrapSpec{
				OSFamily:          OSFamilyDebian,
				Mode:              BootstrapModeManaged,
				KubernetesVersion: "1.34.7",
				PackageRevision:   "1.1",
				Containerd:        ContainerdSpec{AptPin: "2.*"},
			},
		},
	}
}

// TestHashStable pins the hash of a known spec.
//
// A failure here means the hash GENERATOR changed output for identical input,
// which in production would mark every NodeClaim Drifted and roll the whole
// fleet. That is sometimes intended (a new hashed field) and sometimes not (a
// dependency bump changing traversal order). Either way it must be a
// deliberate decision, so update this constant and bump HashVersion together.
func TestHashStable(t *testing.T) {
	const want = "15927006481853939636"
	if got := fixture().Hash(); got != want {
		t.Fatalf("hash changed: got %s, want %s\n\n"+
			"If this change is intended, update the constant AND bump HashVersion, "+
			"or every node in every cluster running this provider will be replaced.", got, want)
	}
}

// TestHashDeterministic guards against map iteration order leaking in.
func TestHashDeterministic(t *testing.T) {
	first := fixture().Hash()
	for i := 0; i < 100; i++ {
		if got := fixture().Hash(); got != first {
			t.Fatalf("hash not deterministic across calls: %s != %s", got, first)
		}
	}
}

// TestHashIgnoresLiveDetectableFields asserts the hash:"ignore" contract.
//
// These fields are all readable back from a live Hetzner server, so drift
// compares them directly against the server. Hashing them too would mean a
// change rolls the fleet via drift AND is caught by comparison, and worse,
// that resolving a selector to a new ID (which happens whenever Hetzner
// rebuilds an image) silently replaces every node.
func TestHashIgnoresLiveDetectableFields(t *testing.T) {
	base := fixture().Hash()

	tests := []struct {
		name   string
		mutate func(*HCloudNodeClassSpec)
	}{
		{"imageSelector", func(s *HCloudNodeClassSpec) { s.ImageSelector = ImageSelectorTerm{ID: lo.ToPtr(int64(42))} }},
		{"imageDriftPolicy", func(s *HCloudNodeClassSpec) { s.ImageDriftPolicy = ImageDriftPolicyReplace }},
		{"locations", func(s *HCloudNodeClassSpec) { s.Locations = []string{"hel1"} }},
		{"networkSelector", func(s *HCloudNodeClassSpec) { s.NetworkSelector = &NetworkSelectorTerm{Name: "other"} }},
		{"firewallSelectors", func(s *HCloudNodeClassSpec) { s.FirewallSelectors = []FirewallSelectorTerm{{Name: "other"}} }},
		{"placementGroup", func(s *HCloudNodeClassSpec) { s.PlacementGroup = &PlacementGroupSelectorTerm{Name: "spread"} }},
		{"publicIPv4", func(s *HCloudNodeClassSpec) { s.PublicIPv4 = lo.ToPtr(false) }},
		{"publicIPv6", func(s *HCloudNodeClassSpec) { s.PublicIPv6 = lo.ToPtr(false) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nc := fixture()
			tt.mutate(&nc.Spec)
			if got := nc.Hash(); got != base {
				t.Errorf("%s is hashed but must not be: hash changed %s -> %s", tt.name, base, got)
			}
		})
	}
}

// TestHashCoversUndetectableFields is the inverse contract: anything hcloud
// will not report back on a live server MUST be hashed, or a change to it
// never reaches existing nodes and the fleet silently diverges from its spec.
func TestHashCoversUndetectableFields(t *testing.T) {
	base := fixture().Hash()

	tests := []struct {
		name   string
		mutate func(*HCloudNodeClassSpec)
	}{
		// hcloud does not return ssh_keys on a server GET.
		{"sshKeySelectors", func(s *HCloudNodeClassSpec) { s.SSHKeySelectors = []SSHKeySelectorTerm{{Name: "another"}} }},
		{"serverLabels", func(s *HCloudNodeClassSpec) { s.ServerLabels = map[string]string{"cost-center": "other"} }},
		// Kubelet feeds both the scheduling model and the node's real flags.
		{"kubelet.kubeReserved", func(s *HCloudNodeClassSpec) {
			s.Kubelet.KubeReserved = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
		}},
		{"kubelet.evictionHard", func(s *HCloudNodeClassSpec) {
			s.Kubelet.EvictionHard = map[string]string{"memory.available": "1Gi"}
		}},
		{"kubelet.maxPods", func(s *HCloudNodeClassSpec) { s.Kubelet.MaxPods = lo.ToPtr(int32(60)) }},
		// user_data is write-only in the hcloud API, so everything rendered
		// into it must be hashed.
		{"bootstrap.kubernetesVersion", func(s *HCloudNodeClassSpec) { s.Bootstrap.KubernetesVersion = "1.35.0" }},
		{"bootstrap.packageRevision", func(s *HCloudNodeClassSpec) { s.Bootstrap.PackageRevision = "1.2" }},
		{"bootstrap.containerd.aptPin", func(s *HCloudNodeClassSpec) { s.Bootstrap.Containerd.AptPin = "3.*" }},
		{"bootstrap.containerd.extraConfig", func(s *HCloudNodeClassSpec) { s.Bootstrap.Containerd.ExtraConfig = "[plugins]" }},
		{"bootstrap.extraPackages", func(s *HCloudNodeClassSpec) { s.Bootstrap.ExtraPackages = []string{"htop"} }},
		{"bootstrap.extraFiles", func(s *HCloudNodeClassSpec) {
			s.Bootstrap.ExtraFiles = []File{{Path: "/etc/example.conf", Content: "x"}}
		}},
		{"bootstrap.preJoinCommands", func(s *HCloudNodeClassSpec) { s.Bootstrap.PreJoinCommands = []string{"echo hi"} }},
		// The deliberate roll-the-fleet lever. If this stops changing the
		// hash, operators lose their only safe way to force a recycle.
		{"bootstrap.revision", func(s *HCloudNodeClassSpec) { s.Bootstrap.Revision = "2026-08-15" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nc := fixture()
			tt.mutate(&nc.Spec)
			if got := nc.Hash(); got == base {
				t.Errorf("%s is not hashed but must be: hash unchanged at %s", tt.name, base)
			}
		})
	}
}

// TestHashSliceOrderInsensitive: reordering a list is not a semantic change
// and must not roll the fleet.
func TestHashSliceOrderInsensitive(t *testing.T) {
	a := fixture()
	a.Spec.Bootstrap.ExtraPackages = []string{"htop", "jq", "curl"}

	b := fixture()
	b.Spec.Bootstrap.ExtraPackages = []string{"curl", "htop", "jq"}

	if a.Hash() != b.Hash() {
		t.Errorf("slice reordering changed the hash: %s != %s", a.Hash(), b.Hash())
	}
}
