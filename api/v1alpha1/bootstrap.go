package v1alpha1

// OSFamily is the node operating system family.
//
// Single-valued today. The kubeadm join, containerd wiring and gVisor setup
// are first-class fields rather than an opaque userData blob, which is what
// makes per-NodeClaim bootstrap tokens, correct taint rendering and a single
// source of truth for kubelet reservations possible. The cost is that this
// provider hard-codes Debian + apt + kubeadm + containerd. The enum exists so
// that adding a family later is an additive change rather than a redesign.
//
// +kubebuilder:validation:Enum=Debian
type OSFamily string

const OSFamilyDebian OSFamily = "Debian"

// BootstrapMode selects how userData is produced.
//
// Single-valued today. A Custom mode taking a raw blob is the obvious future
// addition, once the managed field set has settled.
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

	// KubernetesVersion is the full version to install, e.g. "1.34.7".
	//
	// The apt repository stream is derived from its major.minor, and the
	// pinned package version from the whole string plus PackageRevision.
	// Changing this drifts the fleet, which is the supported upgrade path.
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
	//
	// Normally left empty: the endpoint is read at runtime from the
	// kube-public/cluster-info ConfigMap. Set it when cluster-info advertises
	// an endpoint nodes should not use, such as a public address when nodes
	// should join over the private network.
	//
	// +optional
	APIServerEndpoint string `json:"apiServerEndpoint,omitempty"`

	// CACertHashes overrides the discovered CA public-key pins, in
	// "sha256:<hex>" form.
	//
	// Normally left empty and computed at runtime from cluster-info as the
	// SPKI hash, matching kubeadm's --discovery-token-ca-cert-hash.
	//
	// +optional
	// +kubebuilder:validation:items:Pattern=`^sha256:[a-f0-9]{64}$`
	CACertHashes []string `json:"caCertHashes,omitempty"`

	// +optional
	Containerd ContainerdSpec `json:"containerd,omitempty"`

	// +optional
	GVisor GVisorSpec `json:"gvisor,omitempty"`

	// +optional
	UnattendedUpgrades UnattendedUpgradesSpec `json:"unattendedUpgrades,omitempty"`

	// Sysctls are written to /etc/sysctl.d and applied at boot.
	// +optional
	Sysctls map[string]string `json:"sysctls,omitempty"`

	// KernelModules are loaded at boot via /etc/modules-load.d.
	// +optional
	KernelModules []string `json:"kernelModules,omitempty"`

	// PackageUpgradeOnBoot runs a full package upgrade during cloud-init.
	//
	// Exposed as a field because it is a real trade: it adds roughly one to
	// four minutes to every boot and depends on external mirror speed, while
	// Karpenter core's node registration timeout is a hardcoded 15 minutes. A
	// slow mirror day with this enabled can produce a replacement loop. The
	// durable fix is a prebuilt image; this is the lever until then.
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

	// Revision is an arbitrary string with no effect other than being hashed.
	//
	// It is the deliberate "roll the fleet now" lever: bump it and every
	// NodeClaim drifts, so Karpenter replaces each node one at a time inside
	// the disruption budget and respecting PDBs. Because this provider does
	// not use expireAfter (which is forceful and cannot be budget-gated),
	// this is the primary mechanism for routine node recycling. Without such
	// a field, operators resort to touching a real field to force a roll.
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
	// glob.
	//
	// The default holds containerd within its current major. Unattended
	// upgrades should patch it, but must never cross a major boundary,
	// because that changes the CRI plugin configuration shape and breaks the
	// gVisor shim wiring.
	//
	// +kubebuilder:default="2.*"
	// +optional
	AptPin string `json:"aptPin,omitempty"`

	// ExtraConfig is TOML appended to /etc/containerd/config.toml.
	// +optional
	ExtraConfig string `json:"extraConfig,omitempty"`
}

// GVisorSpec configures the gVisor (runsc) sandbox runtime.
type GVisorSpec struct {
	// Enabled installs runsc and registers it as a containerd runtime handler.
	//
	// Defaults to TRUE. For a general-purpose provider false would be the
	// neutral default, but a RuntimeClass referencing a handler no node
	// provides fails at container-create rather than at scheduling, which is
	// a silent and confusing failure. Defaulting on makes that unreachable by
	// omission.
	//
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// RuntimeHandler is the containerd runtime handler name.
	// +kubebuilder:default=runsc
	// +optional
	RuntimeHandler string `json:"runtimeHandler,omitempty"`

	// Network selects gVisor's network stack.
	//
	// Defaults to host (hostinet). gVisor's own netstack is incompatible with
	// Cilium's kube-proxy replacement.
	//
	// +kubebuilder:validation:Enum=host;sandbox
	// +kubebuilder:default=host
	// +optional
	Network string `json:"network,omitempty"`

	// NodeLabel is set on nodes where runsc is registered, so a RuntimeClass
	// can carry a matching nodeSelector. See LabelGVisor: this is a
	// scheduling aid, not a security attestation.
	//
	// +kubebuilder:default="karpenter.itsh.dev/gvisor"
	// +optional
	NodeLabel string `json:"nodeLabel,omitempty"`
}

// UnattendedUpgradesSpec configures Debian unattended-upgrades.
type UnattendedUpgradesSpec struct {
	// Enabled installs and configures unattended-upgrades.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// ExtraOrigins are additional origin patterns to allow, e.g. entries for
	// third-party repositories whose packages should also auto-patch.
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
