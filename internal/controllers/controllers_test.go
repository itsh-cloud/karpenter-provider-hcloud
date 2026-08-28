package controllers

import (
	"testing"

	"github.com/awslabs/operatorpkg/object"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

// TestNewControllersDoesNotPanic is a real regression guard, not a smoke test.
//
// operatorpkg resolves a GroupVersionKind through k8s.io/client-go's GLOBAL
// scheme with lo.Must, so a type that is registered with the manager's scheme
// but not with that one PANICS rather than returning an error. The panic fires
// while constructing the status controller and again inside karpenter core's
// own wiring, so the binary dies at startup with a stack trace that names
// neither the type nor the missing registration. Constructing the controller
// list is enough to reach it.
func TestNewControllersDoesNotPanic(t *testing.T) {
	kubeClient := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()

	got := NewControllers(
		clock.RealClock{},
		kubeClient,
		events.NewRecorder(record.NewFakeRecorder(10)),
		record.NewFakeRecorder(10),
		nil, nil, nil,
		nil, "test-cluster",
	)
	if len(got) != 4 {
		t.Errorf("NewControllers returned %d controllers, want 4", len(got))
	}
}

// TestNodeClassIsInTheGlobalScheme states the same requirement directly, so a
// failure points at the registration rather than at whatever happened to
// dereference it first. Both the object and its List type are needed: the List
// backs every nodeclaimutils.ForNodeClass lookup.
func TestNodeClassIsInTheGlobalScheme(t *testing.T) {
	gvk := object.GVK(&v1alpha1.HCloudNodeClass{})
	if gvk.Group != v1alpha1.Group || gvk.Kind != "HCloudNodeClass" {
		t.Errorf("GVK = %s, want %s/HCloudNodeClass", gvk, v1alpha1.Group)
	}
	if gvk := object.GVK(&v1alpha1.HCloudNodeClassList{}); gvk.Kind != "HCloudNodeClassList" {
		t.Errorf("list GVK = %s, want kind HCloudNodeClassList", gvk)
	}
}
