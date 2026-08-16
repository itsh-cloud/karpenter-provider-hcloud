package bootstrap

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

const (
	// MaxUserDataBytes is the hcloud API limit on user_data.
	MaxUserDataBytes = 32 * 1024

	// GzipThresholdBytes is when to compress. cloud-init accepts gzipped
	// user-data; staying uncompressed below this keeps it readable on the node
	// and in the hcloud console, which matters when debugging a node that did
	// not join.
	GzipThresholdBytes = 24 * 1024
)

// cloudConfig is the #cloud-config document.
//
// Built as a struct and marshalled rather than rendered from a text template.
// ExtraFiles content is operator-supplied and arbitrary, so templating it
// straight into YAML would let a NodeClass break out of its content block and
// rewrite the rest of the document, including the join configuration.
type cloudConfig struct {
	PackageUpdate  bool        `json:"package_update"`
	PackageUpgrade bool        `json:"package_upgrade"`
	Packages       []string    `json:"packages,omitempty"`
	WriteFiles     []writeFile `json:"write_files,omitempty"`
	Runcmd         []string    `json:"runcmd,omitempty"`
}

type writeFile struct {
	Path        string `json:"path"`
	Permissions string `json:"permissions,omitempty"`
	Owner       string `json:"owner,omitempty"`
	Content     string `json:"content"`
}

// Input is everything needed to render a node's userData.
type Input struct {
	NodeClass *v1alpha1.HCloudNodeClass
	Join      JoinInput
}

// Render produces the cloud-init document for one NodeClaim.
func Render(in Input) (string, error) {
	if in.NodeClass == nil {
		return "", fmt.Errorf("nodeClass is required")
	}
	spec := &in.NodeClass.Spec
	boot := &spec.Bootstrap

	if boot.OSFamily != "" && boot.OSFamily != v1alpha1.OSFamilyDebian {
		return "", fmt.Errorf("unsupported osFamily %q", boot.OSFamily)
	}

	joinYAML, err := renderJoinConfiguration(in.Join)
	if err != nil {
		return "", err
	}

	minor, err := minorVersion(boot.KubernetesVersion)
	if err != nil {
		return "", err
	}
	pkgVersion := boot.KubernetesVersion + "-" + boot.PackageRevisionOrDefault()

	cfg := cloudConfig{
		PackageUpdate:  true,
		PackageUpgrade: boot.PackageUpgradeOnBootEnabled(),
		Packages: dedupe(append([]string{
			"apt-transport-https", "curl", "gnupg", "ca-certificates",
			"unattended-upgrades", "containernetworking-plugins", "nfs-common",
		}, boot.ExtraPackages...)),
	}

	cfg.WriteFiles = append(cfg.WriteFiles,
		writeFile{
			Path:        "/etc/sysctl.d/99-k8s.conf",
			Permissions: "0644",
			Content:     sysctlContent(boot.Sysctls),
		},
		writeFile{
			Path:        "/etc/modules-load.d/k8s.conf",
			Permissions: "0644",
			Content:     modulesContent(boot.KernelModules),
		},
		writeFile{
			Path:        "/etc/apt/preferences.d/containerd",
			Permissions: "0644",
			Content: fmt.Sprintf(`# Bound containerd.io to a major version. unattended-upgrades patches
# freely within it but cannot silently cross a major boundary: that changes
# the CRI plugin configuration shape and breaks any additional runtime
# handler wired into it. Raise deliberately.
Package: containerd.io
Pin: version %s
Pin-Priority: 1001
`, spec.Bootstrap.Containerd.AptPinOrDefault()),
		},
		writeFile{
			Path:        "/etc/default/kubelet",
			Permissions: "0644",
			Content:     "KUBELET_EXTRA_ARGS=--cloud-provider=external\n",
		},
		writeFile{
			Path:        "/etc/kubernetes/kubeadm-join.yaml",
			Permissions: "0600",
			Content:     joinYAML,
		},
	)

	if boot.UnattendedUpgrades.UnattendedUpgradesEnabled() {
		if origins := boot.UnattendedUpgrades.ExtraOrigins; len(origins) > 0 {
			cfg.WriteFiles = append(cfg.WriteFiles, writeFile{
				Path:        "/etc/apt/apt.conf.d/52unattended-upgrades-extra.conf",
				Permissions: "0644",
				Content:     originsContent(origins),
			})
		}
		if boot.UnattendedUpgrades.RemoveUnusedKernelsEnabled() {
			cfg.WriteFiles = append(cfg.WriteFiles, writeFile{
				Path:        "/etc/apt/apt.conf.d/52unattended-upgrades-kernels.conf",
				Permissions: "0644",
				Content: `// Auto-purge superseded kernels so old linux-image-* do not fill /boot.
Unattended-Upgrade::Remove-Unused-Kernel-Packages "true";
`,
			})
		}
	}

	// Operator files last, so they can deliberately override anything above.
	for _, f := range boot.ExtraFiles {
		cfg.WriteFiles = append(cfg.WriteFiles, writeFile{
			Path:        f.Path,
			Permissions: f.Permissions,
			Owner:       f.Owner,
			Content:     f.Content,
		})
	}

	cfg.Runcmd = buildRuncmd(spec, minor, pkgVersion)

	doc, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshalling cloud-config: %w", err)
	}
	out := "#cloud-config\n" + string(doc)

	return maybeCompress(out)
}

func buildRuncmd(spec *v1alpha1.HCloudNodeClassSpec, minor, pkgVersion string) []string {
	boot := &spec.Bootstrap
	cmds := []string{
		"sysctl --system",
		"modprobe br_netfilter",
		"mkdir -p /etc/apt/keyrings",

		// Kubernetes apt repository, pinned to the minor stream.
		fmt.Sprintf("curl -fsSL https://pkgs.k8s.io/core:/stable:/v%s/deb/Release.key "+
			"| gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg", minor),
		fmt.Sprintf("echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] "+
			"https://pkgs.k8s.io/core:/stable:/v%s/deb/ /' > /etc/apt/sources.list.d/kubernetes.list", minor),

		// Docker apt repository, for containerd.io.
		"curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg",
		`echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] ` +
			`https://download.docker.com/linux/debian $(. /etc/os-release && echo \"$VERSION_CODENAME\") stable" ` +
			`> /etc/apt/sources.list.d/docker.list`,

		"apt-get update",
		fmt.Sprintf(`apt-get install -y -o Dpkg::Options::="--force-confold" `+
			`containerd.io kubelet=%s kubeadm=%s kubectl=%s`, pkgVersion, pkgVersion, pkgVersion),
		// Hold, or an unattended upgrade can move the kubelet out from under a
		// pinned control plane.
		"apt-mark hold kubelet kubeadm kubectl",
	}

	if boot.UnattendedUpgrades.UnattendedUpgradesEnabled() {
		cmds = append(cmds, "dpkg-reconfigure -f noninteractive unattended-upgrades")
	}

	cmds = append(cmds,
		"mkdir -p /etc/containerd",
		"containerd config default > /etc/containerd/config.toml",
	)
	if boot.Containerd.SystemdCgroupEnabled() {
		// Must match the kubelet's cgroup driver or the runtime and kubelet
		// disagree about cgroup paths and pods fail to start.
		cmds = append(cmds, `sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml`)
	}
	if extra := strings.TrimSpace(boot.Containerd.ExtraConfig); extra != "" {
		cmds = append(cmds, appendHeredoc("/etc/containerd/config.toml", extra))
	}

	cmds = append(cmds, "systemctl restart containerd", "systemctl enable kubelet")
	cmds = append(cmds, boot.PreJoinCommands...)
	cmds = append(cmds, "kubeadm join --config /etc/kubernetes/kubeadm-join.yaml")
	cmds = append(cmds, boot.PostJoinCommands...)

	return cmds
}

// appendHeredoc appends content to a file without letting it be reinterpreted
// by the shell. The delimiter is quoted, so no expansion happens inside.
func appendHeredoc(path, content string) string {
	const delim = "KARPENTER_EOF"
	return fmt.Sprintf("cat >> %s << '%s'\n%s\n%s", path, delim, content, delim)
}

func sysctlContent(extra map[string]string) string {
	values := map[string]string{
		"net.bridge.bridge-nf-call-iptables":  "1",
		"net.ipv4.ip_forward":                 "1",
		"net.bridge.bridge-nf-call-ip6tables": "1",
		"fs.inotify.max_user_watches":         "524288",
		"fs.inotify.max_user_instances":       "8192",
	}
	for k, v := range extra {
		values[k] = v
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, values[k])
	}
	return b.String()
}

func modulesContent(extra []string) string {
	mods := dedupe(append([]string{"br_netfilter"}, extra...))
	return strings.Join(mods, "\n") + "\n"
}

func originsContent(origins []string) string {
	var b strings.Builder
	b.WriteString("// Additional origins to auto-patch.\n")
	b.WriteString("Unattended-Upgrade::Origins-Pattern {\n")
	for _, o := range origins {
		fmt.Fprintf(&b, "        %q;\n", o)
	}
	b.WriteString("};\n")
	return b.String()
}

// minorVersion turns 1.34.7 into 1.34, which is the apt repository stream.
func minorVersion(version string) (string, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("kubernetesVersion %q is not major.minor.patch", version)
	}
	return parts[0] + "." + parts[1], nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// maybeCompress gzips the document when it approaches the hcloud user_data
// limit, and refuses to return something the API would reject.
func maybeCompress(doc string) (string, error) {
	if len(doc) <= GzipThresholdBytes {
		if len(doc) > MaxUserDataBytes {
			return "", fmt.Errorf("userData is %d bytes, over the %d byte limit", len(doc), MaxUserDataBytes)
		}
		return doc, nil
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(doc)); err != nil {
		return "", fmt.Errorf("compressing userData: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("compressing userData: %w", err)
	}

	if buf.Len() > MaxUserDataBytes {
		return "", fmt.Errorf("userData is %d bytes compressed, over the %d byte limit", buf.Len(), MaxUserDataBytes)
	}
	return buf.String(), nil
}
