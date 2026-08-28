package v1alpha1

import (
	"github.com/awslabs/operatorpkg/status"
)

// Condition types reported on an HCloudNodeClass. Ready rolls these up, and
// Karpenter core gates provisioning on Ready, turning a resolution failure
// into a clear NodeClassNotReady error rather than nodes that cannot join.
const (
	ConditionTypeImageReady              = "ImageReady"
	ConditionTypeNetworkReady            = "NetworkReady"
	ConditionTypeFirewallsReady          = "FirewallsReady"
	ConditionTypeSSHKeysReady            = "SSHKeysReady"
	ConditionTypePlacementGroupReady     = "PlacementGroupReady"
	ConditionTypeBootstrapDiscoveryReady = "BootstrapDiscoveryReady"
	ConditionTypeValidationSucceeded     = "ValidationSucceeded"
)

// HCloudNodeClassStatus is the resolved, observed state of the class.
type HCloudNodeClassStatus struct {
	// Image is the resolved base image.
	// +optional
	Image *ImageStatus `json:"image,omitempty"`

	// Network is the resolved private network.
	// +optional
	Network *NetworkStatus `json:"network,omitempty"`

	// Firewalls are the resolved firewalls.
	// +optional
	Firewalls []FirewallStatus `json:"firewalls,omitempty"`

	// SSHKeys are the resolved SSH keys.
	// +optional
	SSHKeys []SSHKeyStatus `json:"sshKeys,omitempty"`

	// PlacementGroup is the resolved placement group, if any.
	// +optional
	PlacementGroup *PlacementGroupStatus `json:"placementGroup,omitempty"`

	// Locations are the in-scope locations and their datacenters.
	// +optional
	Locations []LocationStatus `json:"locations,omitempty"`

	// APIServerEndpoint is the join endpoint in use, whether discovered from
	// kube-public/cluster-info or overridden in the spec.
	// +optional
	APIServerEndpoint string `json:"apiServerEndpoint,omitempty"`

	// CACertHashes are the CA public-key pins in use. Safe to publish: it is a
	// public key pin, already served anonymously from kube-public/cluster-info
	// to any unauthenticated client.
	// +optional
	CACertHashes []string `json:"caCertHashes,omitempty"`

	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// ImageStatus is a resolved Hetzner image.
type ImageStatus struct {
	ID           int64  `json:"id"`
	Name         string `json:"name,omitempty"`
	Description  string `json:"description,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Created      string `json:"created,omitempty"`
}

// NetworkStatus is a resolved Hetzner private network.
type NetworkStatus struct {
	ID      int64  `json:"id"`
	Name    string `json:"name,omitempty"`
	IPRange string `json:"ipRange,omitempty"`
	// Zone is the network zone, which bounds which locations can attach.
	Zone string `json:"zone,omitempty"`
}

// FirewallStatus is a resolved Hetzner firewall.
type FirewallStatus struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// SSHKeyStatus is a resolved Hetzner SSH key.
type SSHKeyStatus struct {
	ID          int64  `json:"id"`
	Name        string `json:"name,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// PlacementGroupStatus is a resolved Hetzner placement group.
type PlacementGroupStatus struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
	// ServerCount is the current membership. Hetzner caps spread groups at 10
	// and exceeding that returns placement_error on create, so the ceiling is
	// surfaced before it is hit.
	ServerCount int `json:"serverCount,omitempty"`
}

// LocationStatus is an in-scope Hetzner location.
type LocationStatus struct {
	Name        string   `json:"name"`
	NetworkZone string   `json:"networkZone,omitempty"`
	Datacenters []string `json:"datacenters,omitempty"`
}

// StatusConditions implements status.Object.
func (in *HCloudNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions(
		ConditionTypeImageReady,
		ConditionTypeNetworkReady,
		ConditionTypeFirewallsReady,
		ConditionTypeSSHKeysReady,
		ConditionTypePlacementGroupReady,
		ConditionTypeBootstrapDiscoveryReady,
		ConditionTypeValidationSucceeded,
	).For(in, opts...)
}

// GetConditions implements status.Object.
func (in *HCloudNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

// SetConditions implements status.Object.
func (in *HCloudNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}
