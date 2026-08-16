// Package bootstrap renders the cloud-init a new node receives.
package bootstrap

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

// Minimal kubeadm v1beta4 JoinConfiguration. Hand-rolled rather than importing
// k8s.io/kubernetes, which would pull the entire control plane in as a
// dependency to write about thirty lines of YAML.
type joinConfiguration struct {
	APIVersion       string           `json:"apiVersion"`
	Kind             string           `json:"kind"`
	Discovery        discovery        `json:"discovery"`
	NodeRegistration nodeRegistration `json:"nodeRegistration"`
}

type discovery struct {
	BootstrapToken *bootstrapTokenDiscovery `json:"bootstrapToken,omitempty"`
}

type bootstrapTokenDiscovery struct {
	APIServerEndpoint string   `json:"apiServerEndpoint"`
	Token             string   `json:"token"`
	CACertHashes      []string `json:"caCertHashes,omitempty"`
}

type nodeRegistration struct {
	Taints           []corev1.Taint `json:"taints"`
	KubeletExtraArgs []kubeletArg   `json:"kubeletExtraArgs,omitempty"`
}

type kubeletArg struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// JoinInput is everything that varies per NodeClaim.
type JoinInput struct {
	APIServerEndpoint string
	Token             string
	CACertHashes      []string

	// Taints are the NodePool's taints. The karpenter.sh/unregistered taint is
	// added automatically and must not be included here.
	Taints []corev1.Taint

	// NodeLabels are stamped via --node-labels at registration.
	NodeLabels map[string]string

	Kubelet *v1alpha1.KubeletConfiguration
}

// UnregisteredTaint is applied at registration and removed by Karpenter once
// it has stamped its labels onto the Node.
//
// Rendering it is not optional. Without it, pods can be scheduled onto a node
// in the window between the kubelet registering and Karpenter labelling it, so
// Karpenter's bin-packing accounting is wrong from the first second. Karpenter
// core logs an error and proceeds when it is missing, which makes the
// consequence quiet rather than obvious.
var UnregisteredTaint = corev1.Taint{
	Key:    "karpenter.sh/unregistered",
	Effect: corev1.TaintEffectNoExecute,
}

// renderJoinConfiguration produces /etc/kubernetes/kubeadm-join.yaml.
func renderJoinConfiguration(in JoinInput) (string, error) {
	if in.APIServerEndpoint == "" {
		return "", fmt.Errorf("apiServerEndpoint is required")
	}
	if in.Token == "" {
		return "", fmt.Errorf("token is required")
	}
	if len(in.CACertHashes) == 0 {
		// Joining without a CA pin means trusting whatever answers on the
		// endpoint, which is a downgrade we should never make silently.
		return "", fmt.Errorf("caCertHashes is required: joining without a CA pin trusts any endpoint that answers")
	}

	cfg := joinConfiguration{
		APIVersion: "kubeadm.k8s.io/v1beta4",
		Kind:       "JoinConfiguration",
		Discovery: discovery{
			BootstrapToken: &bootstrapTokenDiscovery{
				APIServerEndpoint: in.APIServerEndpoint,
				Token:             in.Token,
				CACertHashes:      in.CACertHashes,
			},
		},
		NodeRegistration: nodeRegistration{
			Taints:           append([]corev1.Taint{UnregisteredTaint}, in.Taints...),
			KubeletExtraArgs: kubeletArgs(in),
		},
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshalling join configuration: %w", err)
	}
	return string(out), nil
}

// kubeletArgs builds nodeRegistration.kubeletExtraArgs.
//
// The reservations here MUST be the same values the instancetype package
// subtracts as InstanceTypeOverhead. If they diverge, Karpenter's model of a
// node and the node itself disagree, it over-packs by the difference, and
// nothing reports it: pods simply sit Pending on a node the scheduler believes
// has room. That is why both read from one KubeletConfiguration.
func kubeletArgs(in JoinInput) []kubeletArg {
	k := in.Kubelet
	args := []kubeletArg{
		{Name: "cloud-provider", Value: "external"},
		{Name: "kube-reserved", Value: resourceListArg(k.KubeReservedOrDefault())},
		{Name: "system-reserved", Value: resourceListArg(k.SystemReservedOrDefault())},
		{Name: "eviction-hard", Value: evictionArg(k.EvictionHardOrDefault())},
		{Name: "max-pods", Value: fmt.Sprint(k.MaxPodsOrDefault())},
	}

	if len(in.NodeLabels) > 0 {
		args = append(args, kubeletArg{Name: "node-labels", Value: mapArg(in.NodeLabels)})
	}
	if len(k.ClusterDNS) > 0 {
		args = append(args, kubeletArg{Name: "cluster-dns", Value: strings.Join(k.ClusterDNS, ",")})
	}
	for _, extra := range k.ExtraArgs {
		args = append(args, kubeletArg{Name: extra.Name, Value: extra.Value})
	}
	return args
}

// resourceListArg renders cpu=200m,memory=512Mi with a stable key order, so
// that an unchanged configuration always produces byte-identical userData and
// does not look like drift.
func resourceListArg(rl corev1.ResourceList) string {
	parts := make([]string, 0, len(rl))
	for name, q := range rl {
		parts = append(parts, fmt.Sprintf("%s=%s", name, q.String()))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// evictionArg renders memory.available<500Mi,nodefs.available<10%,...
//
// Note kubelet's --eviction-hard REPLACES its default map rather than merging,
// so every signal that should remain active has to appear here.
func evictionArg(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for signal, threshold := range m {
		parts = append(parts, fmt.Sprintf("%s<%s", signal, threshold))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func mapArg(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
