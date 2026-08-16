package bootstrap

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

// Provider renders the cloud-init for one NodeClaim, minting the join token it
// carries.
type Provider struct {
	minter      *TokenMinter
	discovery   *Discovery
	clusterName string
}

// NewProvider returns a bootstrap provider.
func NewProvider(minter *TokenMinter, discovery *Discovery, clusterName string) *Provider {
	return &Provider{minter: minter, discovery: discovery, clusterName: clusterName}
}

// Render mints a token and produces the cloud-init document for nodeClaim.
//
// The token is minted ONCE per NodeClaim rather than once per create attempt.
// A NodeClaim that falls through several out-of-stock server types would
// otherwise leave a live cluster-join credential behind for each attempt, all
// of them valid until they expire, and all of them readable from the userdata
// of whichever server did eventually boot.
func (p *Provider) Render(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass, nodeClaim *karpv1.NodeClaim) (string, error) {
	endpoint, hashes := nodeClass.Status.APIServerEndpoint, nodeClass.Status.CACertHashes
	if endpoint == "" || len(hashes) == 0 {
		// Refused rather than rediscovered here. The NodeClass controllers own
		// discovery and gate Ready on it, so an empty value means this
		// NodeClaim is being launched against a class that never resolved, and
		// booting it would produce a server that bills and never joins.
		return "", fmt.Errorf("nodeclass %q has no resolved join parameters", nodeClass.Name)
	}

	token, err := p.minter.Mint(ctx, nodeClaim, p.clusterName)
	if err != nil {
		return "", fmt.Errorf("minting a join token for %q, %w", nodeClaim.Name, err)
	}

	return Render(Input{
		NodeClass: nodeClass,
		Join: JoinInput{
			APIServerEndpoint: endpoint,
			CACertHashes:      hashes,
			Token:             token,
			NodeLabels:        nodeLabels(nodeClaim),
			Taints:            nodeTaints(nodeClaim),
			Kubelet:           &nodeClass.Spec.Kubelet,
		},
	})
}

// nodeLabels are the labels kubeadm stamps on the node at registration.
//
// Rendered per NodeClaim into nodeRegistration rather than applied afterwards:
// karpenter's own syncNode merges labels only after the node has already
// registered, so pods can land in the window before that, and binpacking
// accounting is wrong for every one of them.
func nodeLabels(nodeClaim *karpv1.NodeClaim) map[string]string {
	out := map[string]string{}
	for k, v := range nodeClaim.Labels {
		out[k] = v
	}
	// Not optional, and not merely informational: karpenter matches a Node back
	// to its NodeClaim through this, and a node without it is one core cannot
	// account for.
	out[karpv1.NodePoolLabelKey] = nodeClaim.Labels[karpv1.NodePoolLabelKey]
	if out[karpv1.NodePoolLabelKey] == "" {
		delete(out, karpv1.NodePoolLabelKey)
	}
	return out
}

// nodeTaints are the NodePool's own taints for this node.
//
// The karpenter.sh/unregistered taint is deliberately NOT added here: Render
// adds it itself, and adding it twice would register the node with a duplicate
// taint. Startup taints are included because a node must carry them from the
// moment it registers, not from whenever a controller gets round to it.
func nodeTaints(nodeClaim *karpv1.NodeClaim) []corev1.Taint {
	taints := append([]corev1.Taint{}, nodeClaim.Spec.Taints...)
	return append(taints, nodeClaim.Spec.StartupTaints...)
}
