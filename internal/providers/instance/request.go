package instance

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// corev1RegionLabel is where an offering carries its Hetzner location.
const corev1RegionLabel = corev1.LabelTopologyRegion

// baseRequest builds everything about a create that does not depend on which
// candidate is being attempted.
//
// Built once and reused across the fall-through, which matters for the token:
// the user data carries a live bootstrap token, and minting a fresh one per
// attempt would leave a trail of valid cluster-join credentials for every
// server type that happened to be out of stock.
//
// Every value comes from the NodeClass STATUS, never from its spec. The status
// holds ids the NodeClass controllers already resolved and published, so a
// create cannot race a selector that would resolve differently now, and a
// NodeClass that has not resolved cannot order anything at all.
func (p *Provider) baseRequest(
	nodeClass *v1alpha1.HCloudNodeClass,
	nodeClaim *karpv1.NodeClaim,
	userData string,
) (hcloudapi.CreateServerRequest, error) {
	if nodeClass.Status.Image == nil || nodeClass.Status.Image.ID == 0 {
		return hcloudapi.CreateServerRequest{}, fmt.Errorf("nodeClass %q has no resolved image", nodeClass.Name)
	}
	if p.clusterName == "" {
		// Refused rather than defaulted. This label is the ownership check every
		// delete gates on, so an empty value would produce servers that nothing
		// recognises as its own and that no cleanup path would ever remove.
		return hcloudapi.CreateServerRequest{}, fmt.Errorf("cluster name is empty, refusing to create an unowned server")
	}

	req := hcloudapi.CreateServerRequest{
		// Server name equals NodeClaim name equals, once it joins, Node name.
		// That identity is what makes orphan detection a string compare rather
		// than a join across three systems.
		Name:     nodeClaim.Name,
		ImageID:  nodeClass.Status.Image.ID,
		UserData: userData,
		Labels: map[string]string{
			hcloudapi.LabelManagedBy: p.clusterName,
			hcloudapi.LabelNodeClaim: nodeClaim.Name,
		},
		PublicIPv4: nodeClass.Spec.PublicIPv4Enabled(),
		PublicIPv6: nodeClass.Spec.PublicIPv6Enabled(),
	}

	if pool := nodeClaim.Labels[karpv1.NodePoolLabelKey]; pool != "" {
		req.Labels[hcloudapi.LabelNodePool] = pool
	}
	if net := nodeClass.Status.Network; net != nil && net.ID != 0 {
		req.NetworkIDs = []int64{net.ID}
	}
	for _, fw := range nodeClass.Status.Firewalls {
		req.FirewallIDs = append(req.FirewallIDs, fw.ID)
	}
	for _, key := range nodeClass.Status.SSHKeys {
		req.SSHKeyIDs = append(req.SSHKeyIDs, key.ID)
	}
	if pg := nodeClass.Status.PlacementGroup; pg != nil && pg.ID != 0 {
		id := pg.ID
		req.PlacementGroupID = &id
	}
	for k, v := range nodeClass.Spec.ServerLabels {
		// Operator labels are applied last but must not be able to overwrite
		// ownership: a NodeClass that could set karpenter.sh/managed-by could
		// make its servers invisible to cleanup, or make another cluster's
		// servers look like ours.
		if k == hcloudapi.LabelManagedBy || k == hcloudapi.LabelNodeClaim {
			continue
		}
		req.Labels[k] = v
	}

	return req, nil
}
