package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HCloudNodeClass describes how to build a Hetzner Cloud server that will join
// this cluster as a node.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=hcloudnodeclasses,scope=Cluster,categories=karpenter,shortName={hcnc,hcncs}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image.name`
// +kubebuilder:printcolumn:name="Network",type=string,JSONPath=`.status.network.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HCloudNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HCloudNodeClassSpec   `json:"spec,omitempty"`
	Status HCloudNodeClassStatus `json:"status,omitempty"`
}

// HCloudNodeClassList contains a list of HCloudNodeClass.
//
// +kubebuilder:object:root=true
type HCloudNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HCloudNodeClass `json:"items"`
}

// HCloudNodeClassSpec is the desired shape of a node.
//
// Fields carrying `hash:"ignore"` are readable back from the Hetzner API on a
// live server, so drift compares them against the server rather than a stored
// hash. Everything else is hashed, because hcloud will not return it once the
// server exists: notably user_data (write-only) and ssh_keys (absent from the
// server representation entirely).
type HCloudNodeClassSpec struct {
	// ImageSelector picks the base image. Exactly one of name or id: a name
	// follows Hetzner's periodic image rebuilds, an id pins one build forever.
	//
	// +kubebuilder:validation:Required
	ImageSelector ImageSelectorTerm `json:"imageSelector" hash:"ignore"`

	// ImageDriftPolicy controls whether a changed image ID drifts nodes.
	//
	// Defaults to Ignore, unlike some other providers: Hetzner rebuilds images
	// such as debian-13 periodically and the ID changes each time, so Replace
	// rolls the whole fleet for no benefit, apt having reconfigured the OS at
	// boot anyway. Use Replace only when pinning a snapshot you version.
	//
	// +kubebuilder:validation:Enum=Ignore;Replace
	// +kubebuilder:default=Ignore
	// +optional
	ImageDriftPolicy ImageDriftPolicy `json:"imageDriftPolicy,omitempty" hash:"ignore"`

	// Locations bounds which Hetzner locations this class may use, e.g.
	// [nbg1, fsn1]. Empty means every location in the network's zone. This is
	// the infrastructure bound; a NodePool's topology.kubernetes.io/region
	// requirement is the policy bound and narrows it further.
	//
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:Pattern=`^[a-z]{2,4}[0-9]$`
	// +optional
	Locations []string `json:"locations,omitempty" hash:"ignore"`

	// NetworkSelector picks the private network to attach. Without it a node
	// has no route to the API server's private endpoint.
	//
	// +optional
	NetworkSelector *NetworkSelectorTerm `json:"networkSelector,omitempty" hash:"ignore"`

	// FirewallSelectors picks firewalls to apply.
	//
	// +kubebuilder:validation:MaxItems=5
	// +optional
	FirewallSelectors []FirewallSelectorTerm `json:"firewallSelectors,omitempty" hash:"ignore"`

	// SSHKeySelectors picks SSH keys to install. Hashed rather than compared
	// live, because hcloud does not return ssh_keys on a server GET.
	//
	// +kubebuilder:validation:MaxItems=10
	// +optional
	SSHKeySelectors []SSHKeySelectorTerm `json:"sshKeySelectors,omitempty"`

	// PlacementGroup optionally puts nodes in a spread placement group. Unset
	// by default, deliberately: Hetzner caps spread groups at 10 servers, and
	// the eleventh create returns placement_error, indistinguishable from
	// stock exhaustion and not fixable by trying a different server type.
	//
	// +optional
	PlacementGroup *PlacementGroupSelectorTerm `json:"placementGroup,omitempty" hash:"ignore"`

	// PublicIPv4 controls whether nodes get a primary IPv4, billed separately.
	// Without one, nodes need NAT egress to reach apt and registries. When
	// false, the IPv4 surcharge is dropped from the offering price.
	//
	// +kubebuilder:default=true
	// +optional
	PublicIPv4 *bool `json:"publicIPv4,omitempty" hash:"ignore"`

	// PublicIPv6 controls whether nodes get a primary IPv6.
	//
	// +kubebuilder:default=true
	// +optional
	PublicIPv6 *bool `json:"publicIPv6,omitempty" hash:"ignore"`

	// ServerLabels are extra hcloud labels for cost allocation and filtering.
	// Merged under the provider-managed labels, which always win.
	//
	// +kubebuilder:validation:MaxProperties=32
	// +optional
	ServerLabels map[string]string `json:"serverLabels,omitempty"`

	// Kubelet is the authoritative source for kubelet reservations.
	//
	// These feed BOTH the scheduling model (InstanceTypeOverhead, how Karpenter
	// computes allocatable) AND the flags the kubelet runs with. One field,
	// because if the two diverge Karpenter overpacks every node by the
	// difference and pods sit Pending on nodes it believes have room.
	//
	// +optional
	Kubelet KubeletConfiguration `json:"kubelet,omitempty"`

	// Bootstrap describes how the node installs and joins.
	//
	// +kubebuilder:validation:Required
	Bootstrap BootstrapSpec `json:"bootstrap"`
}

// ImageDriftPolicy controls whether a changed image ID drifts existing nodes.
type ImageDriftPolicy string

const (
	ImageDriftPolicyIgnore  ImageDriftPolicy = "Ignore"
	ImageDriftPolicyReplace ImageDriftPolicy = "Replace"
)

// ImageSelectorTerm selects a Hetzner image by name or id.
//
// +kubebuilder:validation:XValidation:rule="has(self.name) != has(self.id)",message="exactly one of name or id must be set"
type ImageSelectorTerm struct {
	// Name of the image, e.g. debian-13.
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	ID *int64 `json:"id,omitempty"`
}

// NetworkSelectorTerm selects a Hetzner private network by name or id.
//
// +kubebuilder:validation:XValidation:rule="has(self.name) != has(self.id)",message="exactly one of name or id must be set"
type NetworkSelectorTerm struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	ID *int64 `json:"id,omitempty"`
}

// FirewallSelectorTerm selects a Hetzner firewall by name or id.
//
// +kubebuilder:validation:XValidation:rule="has(self.name) != has(self.id)",message="exactly one of name or id must be set"
type FirewallSelectorTerm struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	ID *int64 `json:"id,omitempty"`
}

// SSHKeySelectorTerm selects a Hetzner SSH key by name or id.
//
// +kubebuilder:validation:XValidation:rule="has(self.name) != has(self.id)",message="exactly one of name or id must be set"
type SSHKeySelectorTerm struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	ID *int64 `json:"id,omitempty"`
}

// PlacementGroupSelectorTerm selects a Hetzner placement group by name or id.
//
// +kubebuilder:validation:XValidation:rule="has(self.name) != has(self.id)",message="exactly one of name or id must be set"
type PlacementGroupSelectorTerm struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	ID *int64 `json:"id,omitempty"`
}

// KubeletConfiguration carries the reservations and limits the kubelet runs
// with, and which Karpenter subtracts when computing allocatable.
type KubeletConfiguration struct {
	// KubeReserved is resources reserved for Kubernetes system components.
	// +optional
	KubeReserved corev1.ResourceList `json:"kubeReserved,omitempty"`

	// SystemReserved is resources reserved for OS system daemons.
	// +optional
	SystemReserved corev1.ResourceList `json:"systemReserved,omitempty"`

	// EvictionHard are the hard eviction thresholds. --eviction-hard REPLACES
	// kubelet's entire default map rather than merging into it, so omitting a
	// signal that kubelet defaults to disables that signal entirely.
	//
	// +optional
	EvictionHard map[string]string `json:"evictionHard,omitempty"`

	// EvictionSoft are the soft eviction thresholds.
	// +optional
	EvictionSoft map[string]string `json:"evictionSoft,omitempty"`

	// EvictionSoftGracePeriod is the grace period per soft eviction signal.
	// +optional
	EvictionSoftGracePeriod map[string]metav1.Duration `json:"evictionSoftGracePeriod,omitempty"`

	// MaxPods caps pods per node. Also the "pods" resource Karpenter bin-packs
	// against.
	//
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxPods *int32 `json:"maxPods,omitempty"`

	// ClusterDNS overrides the DNS server addresses passed to the kubelet.
	// +optional
	ClusterDNS []string `json:"clusterDNS,omitempty"`

	// ExtraArgs are additional kubelet flags, rendered into the kubeadm
	// JoinConfiguration's nodeRegistration.kubeletExtraArgs.
	// +optional
	ExtraArgs []KubeletArg `json:"extraArgs,omitempty"`
}

// KubeletArg is a single kubelet flag in kubeadm v1beta4's name/value shape.
type KubeletArg struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +optional
	Value string `json:"value,omitempty"`
}
