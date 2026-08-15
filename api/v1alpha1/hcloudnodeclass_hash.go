package v1alpha1

import (
	"fmt"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
)

const (
	// AnnotationHash carries the spec hash a node was created from.
	AnnotationHash = Group + "/hcloudnodeclass-hash"

	// AnnotationHashVersion carries the HashVersion that produced it.
	AnnotationHashVersion = Group + "/hcloudnodeclass-hash-version"

	// HashVersion is bumped whenever the hash GENERATOR changes its output
	// for identical input: a new hashed field, a change to which fields carry
	// hash:"ignore", or a change to the hashing options below.
	//
	// It exists so that a controller upgrade does not mass-replace the fleet.
	// Drift compares hashes only when both sides carry the same version; when
	// they differ, the hash controller back-fills the new annotation and drift
	// stays quiet until like is being compared with like. An operator who
	// actually wants a roll bumps spec.bootstrap.revision instead.
	HashVersion = "v1"
)

// Hash returns a stable hash of the drift-relevant parts of the spec.
//
// Fields tagged hash:"ignore" are excluded because they are readable back from
// a live Hetzner server and are therefore compared directly. What remains is
// what hcloud will not tell us after the fact: userData (write-only) and
// ssh_keys (absent from the server representation), plus the kubelet
// configuration those render into.
func (in *HCloudNodeClass) Hash() string {
	return fmt.Sprint(lo.Must(hashstructure.Hash(in.Spec, hashstructure.FormatV2, &hashstructure.HashOptions{
		// Slices are order-insensitive: reordering sshKeySelectors or
		// extraPackages is not a semantic change and must not roll the fleet.
		SlicesAsSets: true,
		// A nil *bool and an explicit false hash alike, so adding an explicit
		// default to a manifest does not drift existing nodes.
		ZeroNil: true,
		// Likewise for adding a field that is left at its zero value.
		IgnoreZeroValue: true,
	})))
}
