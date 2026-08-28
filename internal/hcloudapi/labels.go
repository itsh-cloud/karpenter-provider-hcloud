package hcloudapi

import "fmt"

// Labels this provider stamps on every server it creates. These are the entire
// basis for deciding what this controller owns. Hetzner label keys and values
// follow Kubernetes label syntax, so these are valid as written.
const (
	// LabelManagedBy carries the cluster name and is the ownership check. Every
	// destructive path filters on it, and servers this provider did not create,
	// control plane nodes included, genuinely do not carry it: the blast radius
	// of getting it wrong is deleting the control plane. The cluster name
	// rather than a constant, so two clusters sharing one Hetzner project
	// cannot delete each other's nodes.
	LabelManagedBy = "karpenter.sh/managed-by"

	// LabelNodePool is the NodePool that asked for the server. Not used for
	// ownership, only for attribution and for a human reading the console.
	LabelNodePool = "karpenter.sh/nodepool"

	// LabelNodeClaim is the NodeClaim the server belongs to. Redundant with
	// the name today (server name equals NodeClaim name equals Node name), but
	// stamped anyway because adoption after a lost create response matches on
	// it rather than trusting the name alone.
	LabelNodeClaim = "karpenter.itsh.dev/nodeclaim"
)

// ManagedBySelector is the server-side label selector for everything this
// cluster's provider owns.
func ManagedBySelector(clusterName string) string {
	return fmt.Sprintf("%s==%s", LabelManagedBy, clusterName)
}

// IsManagedBy reports whether a server belongs to this cluster's provider.
//
// Fails CLOSED: a server with no labels, or with the key absent, is not ours.
// Every caller that deletes must gate on this.
func (s *Server) IsManagedBy(clusterName string) bool {
	if s == nil || len(s.Labels) == 0 || clusterName == "" {
		return false
	}
	return s.Labels[LabelManagedBy] == clusterName
}
