package cloudprovider

import (
	"context"
	"fmt"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// Drift reasons. These reach the operator through the NodeClaim's Drifted
// condition and karpenter's disruption events, so they name what changed rather
// than the mechanism that noticed.
const (
	// NodeClassDrift means the NodeClass spec changed in a way that would
	// produce a different server. It covers everything hcloud does NOT return
	// on a read, notably user data and ssh keys, which is why it is a hash.
	NodeClassDrift cloudprovider.DriftReason = "NodeClassDrift"

	LocationDrift       cloudprovider.DriftReason = "LocationDrift"
	NetworkDrift        cloudprovider.DriftReason = "NetworkDrift"
	FirewallDrift       cloudprovider.DriftReason = "FirewallDrift"
	PlacementGroupDrift cloudprovider.DriftReason = "PlacementGroupDrift"
	PublicNetDrift      cloudprovider.DriftReason = "PublicNetDrift"
	ImageDrift          cloudprovider.DriftReason = "ImageDrift"
)

// IsDrifted reports whether a NodeClaim no longer matches what its NodeClass
// would produce today.
//
// Deliberately not covered: the server TYPE, because correcting a wrongly-typed
// node is consolidation's job and treating it as drift would have the two
// fighting over the same node; and the datacenter, because servers are ordered
// by LOCATION and Hetzner picks the datacenter within it, so comparing one would
// mark every node permanently drifted for a value we never asked for.
func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	// A NodeClaim that has not launched has nothing to compare. Reporting drift
	// here would ask core to replace a node that does not exist yet.
	if nodeClaim.Status.ProviderID == "" {
		return "", nil
	}

	nodeClass := &v1alpha1.HCloudNodeClass{}
	if err := c.kubeClient.Get(ctx, nodeClassKey(nodeClaim), nodeClass); err != nil {
		return "", fmt.Errorf("getting nodeclass for drift evaluation, %w", err)
	}
	// A terminating NodeClass is already being removed; declaring its nodes
	// drifted would race the termination gate for no benefit.
	if !nodeClass.DeletionTimestamp.IsZero() {
		return "", nil
	}
	// The resolvers NIL the status fields they cannot resolve, and comparing
	// against that reads as "everything changed", drifting the whole fleet on
	// one bad selector. Ready is the controller's statement that its status is
	// worth reading, so drift declines without it, exactly as Create does.
	if !nodeClass.StatusConditions().Root().IsTrue() {
		log.FromContext(ctx).V(1).Info("skipping drift evaluation: the nodeclass is not ready, so its status is not comparable",
			"nodeclass", nodeClass.Name)
		return "", nil
	}

	if reason := nodeClassHashDrift(ctx, nodeClass, nodeClaim); reason != "" {
		return reason, nil
	}

	srv, err := c.instances.Get(ctx, nodeClaim.Status.ProviderID)
	if err != nil {
		// Transient. Returning a drift reason on a failed read would replace a
		// node because the API was briefly unreachable.
		return "", fmt.Errorf("getting server for drift evaluation, %w", err)
	}
	if srv == nil {
		// Gone, or not ours: the garbage collector's business. Replacing it
		// would create a second server for a NodeClaim whose first vanished.
		return "", nil
	}

	return serverDrift(nodeClass, srv), nil
}

// nodeClassHashDrift compares the spec hash the NodeClaim was built with
// against the NodeClass's current one.
//
// Gated on the hash VERSION matching on both sides, which is what makes it safe
// during an upgrade: a changed generator produces a different hash for an
// unchanged spec, so comparing across versions would replace the whole fleet at
// once. Until the back-fill makes both sides agree, this declines to judge.
func nodeClassHashDrift(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass, nodeClaim *karpv1.NodeClaim) cloudprovider.DriftReason {
	classVersion := nodeClass.Annotations[v1alpha1.AnnotationHashVersion]
	claimVersion := nodeClaim.Annotations[v1alpha1.AnnotationHashVersion]
	if classVersion != v1alpha1.HashVersion || claimVersion != v1alpha1.HashVersion {
		log.FromContext(ctx).V(1).Info("skipping nodeclass hash drift: the hash versions do not agree yet",
			"nodeclass", nodeClass.Name, "nodeclassVersion", classVersion, "nodeclaimVersion", claimVersion)
		return ""
	}

	classHash, ok := nodeClass.Annotations[v1alpha1.AnnotationHash]
	if !ok {
		return ""
	}
	claimHash, ok := nodeClaim.Annotations[v1alpha1.AnnotationHash]
	if !ok {
		// Never stamped, so not evidence of drift: replacing on absence would
		// roll the fleet on a slow reconcile.
		return ""
	}
	if classHash != claimHash {
		return NodeClassDrift
	}
	return ""
}

// serverDrift compares the running server against what the NodeClass resolves
// to now.
//
// Every comparison here reads a field hcloud actually returns; anything it does
// not return is covered by the spec hash instead.
func serverDrift(nodeClass *v1alpha1.HCloudNodeClass, srv *hcloudapi.Server) cloudprovider.DriftReason {
	// Location, and ONLY when the operator wrote the list down. Left unset,
	// Status.Locations comes entirely from the catalog, so a partial response or
	// a type going deprecated in one location silently narrows it, and treating
	// that as drift would replace every node there on one odd API response. An
	// operator removing a location is intent; the catalog moving is not.
	if locs := nodeClass.Status.Locations; len(nodeClass.Spec.Locations) > 0 && len(locs) > 0 && srv.Location != "" {
		if !slices.ContainsFunc(locs, func(l v1alpha1.LocationStatus) bool { return l.Name == srv.Location }) {
			return LocationDrift
		}
	}

	switch net := nodeClass.Status.Network; {
	case net == nil:
		// The NodeClass no longer asks for a private network, but the server is
		// attached to one. That is a real change.
		if len(srv.NetworkIDs) > 0 {
			return NetworkDrift
		}
	case net.ID == 0:
		// Present but unresolved, so nothing can be concluded. Folding this
		// into the nil case would read "the operator removed the network" from
		// "we do not know yet", and replace the node for it.
	default:
		if !slices.Contains(srv.NetworkIDs, net.ID) {
			return NetworkDrift
		}
	}

	// Firewalls compare as a SET, not a sequence: hcloud returns them in its own
	// order, and reordering the selectors means nothing.
	want := make([]int64, 0, len(nodeClass.Status.Firewalls))
	for _, fw := range nodeClass.Status.Firewalls {
		want = append(want, fw.ID)
	}
	if !sameSet(want, srv.FirewallIDs) {
		return FirewallDrift
	}

	var wantPG int64
	if pg := nodeClass.Status.PlacementGroup; pg != nil {
		wantPG = pg.ID
	}
	if wantPG != srv.PlacementGroupID {
		return PlacementGroupDrift
	}

	if nodeClass.Spec.PublicIPv4Enabled() != srv.HasPublicIPv4 || nodeClass.Spec.PublicIPv6Enabled() != srv.HasPublicIPv6 {
		return PublicNetDrift
	}

	// Image only when the operator asked for it. Hetzner rebuilds its named
	// images every few weeks, so under the default Ignore policy that must not
	// roll the fleet on Hetzner's schedule. Replace is for people who want it.
	if nodeClass.Spec.ImageDriftPolicyOrDefault() == v1alpha1.ImageDriftPolicyReplace {
		if img := nodeClass.Status.Image; img != nil && img.ID != 0 && srv.ImageID != 0 && img.ID != srv.ImageID {
			return ImageDrift
		}
	}

	return ""
}

// sameSet reports whether two id sets hold the same members, ignoring order and
// duplicates.
func sameSet(a, b []int64) bool {
	x := slices.Sorted(slices.Values(a))
	y := slices.Sorted(slices.Values(b))
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}
