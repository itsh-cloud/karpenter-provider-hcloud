package v1alpha1

// OSFamily is the node operating system family.
//
// Single-valued today: first-class kubeadm and containerd fields, rather than
// an opaque userData blob, are what allow per-NodeClaim bootstrap tokens,
// correct taint rendering and one source of truth for kubelet reservations.
// The cost is hard-coding Debian, apt, kubeadm and containerd. Anything else
// goes through ExtraPackages, ExtraFiles (written before runcmd, so an apt
// source and its key land in time) and the join hooks.
//
// +kubebuilder:validation:Enum=Debian
type OSFamily string

const OSFamilyDebian OSFamily = "Debian"

// BootstrapMode selects how userData is produced. Single-valued today; a
// Custom mode taking a raw blob is the obvious future addition.
//
// +kubebuilder:validation:Enum=Managed
type BootstrapMode string

const BootstrapModeManaged BootstrapMode = "Managed"

// BootstrapSpec describes how a node installs its components and joins.
type BootstrapSpec struct {
	// +kubebuilder:default=Debian
	// +optional
	OSFamily OSFamily `json:"osFamily,omitempty"`

	// +kubebuilder:default=Managed
	// +optional
	Mode BootstrapMode `json:"mode,omitempty"`

	// KubernetesVersion is the full version to install, e.g. "1.34.7". The apt
	// repository stream comes from its major.minor and the pinned package
	// version from the whole string plus PackageRevision. Changing it drifts
	// the fleet, which is the supported upgrade path.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	KubernetesVersion string `json:"kubernetesVersion"`

	// PackageRevision is the Debian package revision suffix, e.g. "1.1",
	// producing kubelet=1.34.7-1.1.
	//
	// +kubebuilder:default="1.1"
	// +optional
	PackageRevision string `json:"packageRevision,omitempty"`

	// APIServerEndpoint overrides the discovered join endpoint, as host:port.
	// Normally empty: it is read at runtime from kube-public/cluster-info. Set
	// it when cluster-info advertises an endpoint nodes should not use, such as
	// a public address where nodes should join over the private network.
	//
	// +optional
	APIServerEndpoint string `json:"apiServerEndpoint,omitempty"`

	// CACertHashes overrides the discovered CA public-key pins, in
	// "sha256:<hex>" form. Normally empty and computed at runtime from
	// cluster-info as the SPKI hash, matching kubeadm's
	// --discovery-token-ca-cert-hash.
	//
	// +optional
	// +kubebuilder:validation:items:Pattern=`^sha256:[a-f0-9]{64}$`
	CACertHashes []string `json:"caCertHashes,omitempty"`

	// +optional
	Containerd ContainerdSpec `json:"containerd,omitempty"`

	// +optional
	UnattendedUpgrades UnattendedUpgradesSpec `json:"unattendedUpgrades,omitempty"`

	// Sysctls are written to /etc/sysctl.d and applied at boot.
	// +optional
	Sysctls map[string]string `json:"sysctls,omitempty"`

	// KernelModules are loaded at boot via /etc/modules-load.d.
	// +optional
	KernelModules []string `json:"kernelModules,omitempty"`

	// PackageUpgradeOnBoot runs a full package upgrade during cloud-init. It
	// adds one to four minutes to every boot and depends on mirror speed,
	// against karpenter core's hardcoded 15-minute registration timeout, so a
	// slow mirror day can produce a replacement loop. The durable fix is a
	// prebuilt image; this is the lever until then.
	//
	// +kubebuilder:default=true
	// +optional
	PackageUpgradeOnBoot *bool `json:"packageUpgradeOnBoot,omitempty"`

	// ExtraPackages are additional apt packages to install.
	// +optional
	ExtraPackages []string `json:"extraPackages,omitempty"`

	// ExtraFiles are additional files to write during cloud-init.
	// +optional
	ExtraFiles []File `json:"extraFiles,omitempty"`

	// PreJoinCommands run after packages are configured, before kubeadm join.
	// +optional
	PreJoinCommands []string `json:"preJoinCommands,omitempty"`

	// PostJoinCommands run after a successful kubeadm join.
	// +optional
	PostJoinCommands []string `json:"postJoinCommands,omitempty"`

	// Revision is an arbitrary string with no effect other than being hashed:
	// the deliberate "roll the fleet now" lever. Bump it and every NodeClaim
	// drifts, so Karpenter replaces nodes one at a time inside the disruption
	// budget and respecting PDBs. This provider does not use expireAfter,
	// which is forceful and cannot be budget-gated.
	//
	// +optional
	Revision string `json:"revision,omitempty"`
}

// ContainerdSpec configures the container runtime.
type ContainerdSpec struct {
	// SystemdCgroup sets the systemd cgroup driver.
	// +kubebuilder:default=true
	// +optional
	SystemdCgroup *bool `json:"systemdCgroup,omitempty"`

	// AptPin constrains the containerd.io package version, as an apt version
	// glob. The default holds containerd within its current major: unattended
	// upgrades should patch it, but crossing a major changes the CRI plugin
	// configuration shape and breaks the additional runtime handler wired into
	// it.
	//
	// +kubebuilder:default="2.*"
	// +optional
	AptPin string `json:"aptPin,omitempty"`

	// ExtraConfig is TOML appended to /etc/containerd/config.toml before
	// containerd first starts, which is where additional runtime handlers go.
	// The CRI plugin's config path changed between containerd majors, so the
	// stanza must match whatever AptPin selects.
	//
	// +optional
	ExtraConfig string `json:"extraConfig,omitempty"`
}

// UnattendedUpgradesSpec configures Debian unattended-upgrades.
type UnattendedUpgradesSpec struct {
	// Enabled installs and configures unattended-upgrades.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// ExtraOrigins are additional origin patterns to allow, e.g. third-party
	// repositories whose packages should also auto-patch.
	// +optional
	ExtraOrigins []string `json:"extraOrigins,omitempty"`

	// RemoveUnusedKernelPackages lets unattended-upgrades purge superseded
	// kernels, which otherwise fill /boot.
	// +kubebuilder:default=true
	// +optional
	RemoveUnusedKernelPackages *bool `json:"removeUnusedKernelPackages,omitempty"`
}

// File is a file written during cloud-init.
type File struct {
	// +kubebuilder:validation:Required
	Path string `json:"path"`

	// +kubebuilder:validation:Required
	Content string `json:"content"`

	// Permissions is an octal mode string, e.g. "0644".
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	// +kubebuilder:default="0644"
	// +optional
	Permissions string `json:"permissions,omitempty"`

	// +kubebuilder:default=root
	// +optional
	Owner string `json:"owner,omitempty"`
}
