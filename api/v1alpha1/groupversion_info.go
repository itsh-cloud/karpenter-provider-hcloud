// Package v1alpha1 contains the HCloudNodeClass API for the Hetzner Cloud
// Karpenter provider.
//
// +kubebuilder:object:generate=true
// +groupName=karpenter.itsh.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// Group is the API group for this provider's types.
//
// Deliberately not karpenter.hcloud.cloud or hcloud.karpenter.sh: both squat
// namespaces owned by others, and nodeClassRef.group is effectively permanent
// once NodePools reference it.
const Group = "karpenter.itsh.dev"

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}

	// SchemeBuilder registers this API's types into a Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
