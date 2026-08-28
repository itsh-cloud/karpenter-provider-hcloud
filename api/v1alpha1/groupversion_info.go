// Package v1alpha1 contains the HCloudNodeClass API for the Hetzner Cloud
// Karpenter provider.
//
// +kubebuilder:object:generate=true
// +groupName=karpenter.itsh.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crscheme "sigs.k8s.io/controller-runtime/pkg/scheme"
)

// Group is the API group for this provider's types.
//
// Deliberately not karpenter.hcloud.cloud or hcloud.karpenter.sh: both squat
// namespaces owned by others, and nodeClassRef.group is effectively permanent
// once NodePools reference it.
const Group = "karpenter.itsh.dev"

// TerminationFinalizer holds an HCloudNodeClass open until nothing references
// it any more.
//
// Karpenter core adds no finalizer to a NodeClass and ships no NodeClass
// termination controller (karpv1.TerminationFinalizer is for NodeClaims and
// Nodes), so keeping a NodeClass alive under live NodeClaims is entirely a
// provider concern.
const TerminationFinalizer = Group + "/termination"

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}

	// SchemeBuilder registers this API's types into a Scheme.
	SchemeBuilder = &crscheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&HCloudNodeClass{}, &HCloudNodeClassList{})

	// Registering into client-go's GLOBAL scheme is a second, separate
	// registration, and it is not optional: operatorpkg's object.GVK resolves
	// through k8s.io/client-go/kubernetes/scheme with lo.Must, so an
	// unregistered type PANICS rather than erroring, and karpenter core calls
	// it while wiring its controllers. controller-runtime's manager also
	// defaults its scheme to this one when Options.Scheme is nil, which
	// karpenter core leaves it.
	metav1.AddToGroupVersion(clientgoscheme.Scheme, GroupVersion)
	clientgoscheme.Scheme.AddKnownTypes(GroupVersion, &HCloudNodeClass{}, &HCloudNodeClassList{})
}
