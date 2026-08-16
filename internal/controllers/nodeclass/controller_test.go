package nodeclass

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/bootstrap"
)

func testClock() clock.Clock {
	return clocktesting.NewFakeClock(time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC))
}

// newNodeClass returns a class exercising every selector, so that a test which
// does not care about a particular one still proves it does not get in the way.
func newNodeClass() *v1alpha1.HCloudNodeClass {
	return &v1alpha1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default", UID: types.UID("nc-uid")},
		Spec: v1alpha1.HCloudNodeClassSpec{
			ImageSelector:     v1alpha1.ImageSelectorTerm{Name: "debian-13"},
			Locations:         []string{"nbg1", "fsn1"},
			NetworkSelector:   &v1alpha1.NetworkSelectorTerm{Name: "cluster"},
			FirewallSelectors: []v1alpha1.FirewallSelectorTerm{{Name: "nodes"}},
			SSHKeySelectors:   []v1alpha1.SSHKeySelectorTerm{{Name: "ops"}},
			PlacementGroup:    &v1alpha1.PlacementGroupSelectorTerm{Name: "spread"},
			Bootstrap:         v1alpha1.BootstrapSpec{KubernetesVersion: "1.34.7"},
		},
	}
}

// minimalNodeClass omits every optional selector.
func minimalNodeClass() *v1alpha1.HCloudNodeClass {
	return &v1alpha1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "minimal", UID: types.UID("min-uid")},
		Spec: v1alpha1.HCloudNodeClassSpec{
			ImageSelector: v1alpha1.ImageSelectorTerm{Name: "debian-13"},
			Bootstrap:     v1alpha1.BootstrapSpec{KubernetesVersion: "1.34.7"},
		},
	}
}

// newFakeClient builds a client with the three spec.nodeClassRef field indexes
// karpenter core registers on a real manager. Without them
// nodeclaimutils.ForNodeClass returns an empty list rather than an error, so
// termination would release the finalizer while NodeClaims still exist: the
// exact bug the finalizer is there to prevent, invisible in any test that skips
// this.
func newFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.HCloudNodeClass{}).
		WithIndex(&karpv1.NodeClaim{}, "spec.nodeClassRef.group", func(o client.Object) []string {
			return []string{o.(*karpv1.NodeClaim).Spec.NodeClassRef.Group}
		}).
		WithIndex(&karpv1.NodeClaim{}, "spec.nodeClassRef.kind", func(o client.Object) []string {
			return []string{o.(*karpv1.NodeClaim).Spec.NodeClassRef.Kind}
		}).
		WithIndex(&karpv1.NodeClaim{}, "spec.nodeClassRef.name", func(o client.Object) []string {
			return []string{o.(*karpv1.NodeClaim).Spec.NodeClassRef.Name}
		}).
		Build()
}

type harness struct {
	t          *testing.T
	client     *countingClient
	recorder   *fakeRecorder
	resources  *fakeResources
	discovery  *fakeDiscovery
	catalog    *fakeCatalog
	controller *Controller
	name       types.NamespacedName
}

func newHarness(t *testing.T, nodeClass *v1alpha1.HCloudNodeClass, extra ...client.Object) *harness {
	t.Helper()
	objs := append([]client.Object{nodeClass}, extra...)
	h := &harness{
		t:         t,
		client:    &countingClient{Client: newFakeClient(objs...)},
		recorder:  &fakeRecorder{},
		resources: &fakeResources{},
		discovery: workingDiscovery(),
		catalog:   twoLocationCatalog(),
		name:      types.NamespacedName{Name: nodeClass.Name},
	}
	h.controller = NewController(testClock(), h.client, h.recorder, h.resources, h.catalog, h.discovery)
	return h
}

// reconcile fetches the object the way reconcile.AsReconciler does, runs one
// pass, and returns the persisted result.
func (h *harness) reconcile(ctx context.Context) (*v1alpha1.HCloudNodeClass, error) {
	h.t.Helper()
	var nc v1alpha1.HCloudNodeClass
	if err := h.client.Get(ctx, h.name, &nc); err != nil {
		h.t.Fatalf("get nodeclass: %v", err)
	}
	if _, err := h.controller.Reconcile(ctx, &nc); err != nil {
		return nil, err
	}
	var out v1alpha1.HCloudNodeClass
	if err := h.client.Get(ctx, h.name, &out); err != nil {
		h.t.Fatalf("get nodeclass after reconcile: %v", err)
	}
	return &out, nil
}

func condition(nc *v1alpha1.HCloudNodeClass, t string) *status.Condition {
	return nc.StatusConditions(status.WithObservedOnly()).Get(t)
}

func assertCondition(t *testing.T, nc *v1alpha1.HCloudNodeClass, conditionType string, want metav1.ConditionStatus, wantReason string) {
	t.Helper()
	got := condition(nc, conditionType)
	if got == nil {
		t.Fatalf("condition %s: not set", conditionType)
	}
	if got.Status != want {
		t.Errorf("condition %s: status = %s, want %s (reason %q, message %q)", conditionType, got.Status, want, got.Reason, got.Message)
	}
	if wantReason != "" && got.Reason != wantReason {
		t.Errorf("condition %s: reason = %q, want %q", conditionType, got.Reason, wantReason)
	}
}

func TestReconcileResolvesEverySelector(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if nc.Status.Image == nil || nc.Status.Image.ID != 42 {
		t.Errorf("status.image = %+v, want id 42", nc.Status.Image)
	}
	if nc.Status.Network == nil || nc.Status.Network.Zone != "eu-central" {
		t.Errorf("status.network = %+v, want zone eu-central", nc.Status.Network)
	}
	if len(nc.Status.Firewalls) != 1 || len(nc.Status.SSHKeys) != 1 {
		t.Errorf("status firewalls = %v, sshKeys = %v, want one each", nc.Status.Firewalls, nc.Status.SSHKeys)
	}
	if nc.Status.PlacementGroup == nil || nc.Status.PlacementGroup.ServerCount != 1 {
		t.Errorf("status.placementGroup = %+v, want serverCount 1", nc.Status.PlacementGroup)
	}
	if nc.Status.APIServerEndpoint != "10.0.0.2:6443" || len(nc.Status.CACertHashes) != 1 {
		t.Errorf("status join parameters = %q / %v", nc.Status.APIServerEndpoint, nc.Status.CACertHashes)
	}
	if len(nc.Status.Locations) != 2 {
		t.Errorf("status.locations = %+v, want nbg1 and fsn1", nc.Status.Locations)
	}

	for _, ct := range append(requiredConditions(), v1alpha1.ConditionTypeValidationSucceeded, status.ConditionReady) {
		assertCondition(t, nc, ct, metav1.ConditionTrue, "")
	}

	// The finalizer is added in the same pass, because a class that is already
	// resolving is a class NodePools can already reference.
	if !hasFinalizer(nc) {
		t.Errorf("finalizers = %v, want %s", nc.Finalizers, v1alpha1.TerminationFinalizer)
	}
}

func hasFinalizer(nc *v1alpha1.HCloudNodeClass) bool {
	for _, f := range nc.Finalizers {
		if f == v1alpha1.TerminationFinalizer {
			return true
		}
	}
	return false
}

// TestReconcileSelectorNotFound covers the configuration branch for every
// resolvable selector: a resolver that answers "nothing matches" must produce a
// False condition naming the selector, and must NOT be reported as an error,
// because retrying with backoff cannot conjure the missing resource and an
// error would bury the reason in the controller's logs.
func TestReconcileSelectorNotFound(t *testing.T) {
	for _, tc := range []struct {
		name          string
		arrange       func(*fakeResources)
		conditionType string
		wantReason    string
	}{
		{
			name: "image",
			arrange: func(r *fakeResources) {
				r.image = func() (*hcloudapi.Image, error) {
					return nil, &hcloudapi.NotFoundError{Kind: "image", Selector: "debian-13"}
				}
			},
			conditionType: v1alpha1.ConditionTypeImageReady,
			wantReason:    "ImageNotFound",
		},
		{
			name: "network",
			arrange: func(r *fakeResources) {
				r.network = func() (*hcloudapi.Network, error) {
					return nil, &hcloudapi.NotFoundError{Kind: "network", Selector: "cluster"}
				}
			},
			conditionType: v1alpha1.ConditionTypeNetworkReady,
			wantReason:    "NetworkNotFound",
		},
		{
			name: "firewall",
			arrange: func(r *fakeResources) {
				r.firewall = func(string) (*hcloudapi.Firewall, error) {
					return nil, &hcloudapi.NotFoundError{Kind: "firewall", Selector: "nodes"}
				}
			},
			conditionType: v1alpha1.ConditionTypeFirewallsReady,
			wantReason:    "FirewallNotFound",
		},
		{
			// A missing ssh key does NOT belong in this table: it fails open.
			// See TestMissingSSHKeyDoesNotStopProvisioning. A rejected
			// credential still fails closed, because that is not a statement
			// about ssh keys.
			name: "sshKeyRejectedCredential",
			arrange: func(r *fakeResources) {
				r.sshKey = func(string) (*hcloudapi.SSHKey, error) { return nil, fatalErr }
			},
			conditionType: v1alpha1.ConditionTypeSSHKeysReady,
			wantReason:    "SSHKeyCredentialRejected",
		},
		{
			name: "placementGroup",
			arrange: func(r *fakeResources) {
				r.placementGroup = func() (*hcloudapi.PlacementGroup, error) {
					return nil, &hcloudapi.NotFoundError{Kind: "placement group", Selector: "spread"}
				}
			},
			conditionType: v1alpha1.ConditionTypePlacementGroupReady,
			wantReason:    "PlacementGroupNotFound",
		},
		{
			name: "rejectedCredential",
			arrange: func(r *fakeResources) {
				r.image = func() (*hcloudapi.Image, error) { return nil, fatalErr }
			},
			conditionType: v1alpha1.ConditionTypeImageReady,
			wantReason:    "ImageCredentialRejected",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t, newNodeClass())
			tc.arrange(h.resources)

			nc, err := h.reconcile(ctx)
			if err != nil {
				t.Fatalf("reconcile returned an error for a configuration failure: %v", err)
			}

			assertCondition(t, nc, tc.conditionType, metav1.ConditionFalse, tc.wantReason)
			// Validation short-circuits on a False dependency rather than
			// evaluating anything, and Ready reports the failure rather than
			// waiting to find out.
			assertCondition(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, reasonDependenciesNotReady)
			assertCondition(t, nc, status.ConditionReady, metav1.ConditionFalse, "UnhealthyDependents")
		})
	}
}

// TestReconcileTransportErrorIsRetriedNotReported is the inverse of the case
// above. A failed call must come back as a reconcile error so it is retried
// with backoff, and must leave the condition alone. Setting it False would be
// reported by karpenter core as NodeClassReady=False on every NodePool using
// the class, which deletes in-flight NodeClaims, over an API blip.
func TestReconcileTransportErrorIsRetriedNotReported(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())
	h.resources.image = func() (*hcloudapi.Image, error) { return nil, transientErr }

	if _, err := h.reconcile(ctx); err == nil {
		t.Fatal("reconcile returned nil for a transport failure, so it will not be retried")
	}

	var nc v1alpha1.HCloudNodeClass
	if err := h.client.Get(ctx, h.name, &nc); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Unknown from initialisation, not False: nothing is known to be wrong with
	// the configuration.
	assertCondition(t, &nc, v1alpha1.ConditionTypeImageReady, metav1.ConditionUnknown, "")
	assertCondition(t, &nc, status.ConditionReady, metav1.ConditionUnknown, "ReconcilingDependents")
}

// TestTransientFailureDoesNotUnresolveAHealthyClass is the case that actually
// bites in production: the class resolved fine an hour ago and Hetzner returns
// a 503 now. Ready must stay True, because nothing about the configuration
// changed and taking it False would disrupt running nodes.
func TestTransientFailureDoesNotUnresolveAHealthyClass(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())

	if _, err := h.reconcile(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	h.resources.image = func() (*hcloudapi.Image, error) { return nil, transientErr }
	if _, err := h.reconcile(ctx); err == nil {
		t.Fatal("reconcile returned nil for a transport failure")
	}

	var nc v1alpha1.HCloudNodeClass
	if err := h.client.Get(ctx, h.name, &nc); err != nil {
		t.Fatalf("get: %v", err)
	}
	assertCondition(t, &nc, v1alpha1.ConditionTypeImageReady, metav1.ConditionTrue, "")
	assertCondition(t, &nc, status.ConditionReady, metav1.ConditionTrue, "")
	if nc.Status.Image == nil {
		t.Error("status.image was cleared by a transport failure")
	}
}

// TestOptionalSelectorsUnsetReachReady is the roll-up trap. An optional
// selector left unset must resolve True, not Unknown: an Unknown dependent pins
// Ready at Unknown forever, which karpenter core turns into
// NodeClassReadinessUnknown on the NodePool and which blocks provisioning as
// completely as a hard failure, with a message that names nothing.
func TestOptionalSelectorsUnsetReachReady(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, minimalNodeClass())

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, ct := range []string{
		v1alpha1.ConditionTypeNetworkReady,
		v1alpha1.ConditionTypeFirewallsReady,
		v1alpha1.ConditionTypeSSHKeysReady,
		v1alpha1.ConditionTypePlacementGroupReady,
	} {
		assertCondition(t, nc, ct, metav1.ConditionTrue, "")
	}
	assertCondition(t, nc, status.ConditionReady, metav1.ConditionTrue, "")

	if nc.Status.Network != nil || nc.Status.PlacementGroup != nil {
		t.Errorf("unset selectors published a resolved value: network=%+v placementGroup=%+v", nc.Status.Network, nc.Status.PlacementGroup)
	}
	// With no private network there is no zone bound, so every catalog location
	// is in scope, including the one in another network zone.
	if len(nc.Status.Locations) != 3 {
		t.Errorf("status.locations = %+v, want all three catalog locations", nc.Status.Locations)
	}
}

// TestReadyRollupRequiresEveryDependent walks Ready from False back to True by
// fixing the single thing that was wrong, proving the roll-up is not latched.
func TestReadyRollupRequiresEveryDependent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())
	// A configuration failure, not a blip: the ConfigMap is absent, or the Role
	// was never installed. Only that form is published on the object.
	h.discovery.refreshErr = bootstrap.NewConfigError(errors.New("configmaps \"cluster-info\" is forbidden"))

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertCondition(t, nc, v1alpha1.ConditionTypeBootstrapDiscoveryReady, metav1.ConditionFalse, "ClusterInfoUnavailable")
	assertCondition(t, nc, status.ConditionReady, metav1.ConditionFalse, "UnhealthyDependents")
	if nc.Status.APIServerEndpoint != "" {
		t.Errorf("status.apiServerEndpoint = %q, want empty while discovery is failing", nc.Status.APIServerEndpoint)
	}

	h.discovery.refreshErr = nil
	nc, err = h.reconcile(ctx)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	assertCondition(t, nc, v1alpha1.ConditionTypeBootstrapDiscoveryReady, metav1.ConditionTrue, "")
	assertCondition(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
}

// TestBootstrapDiscoveryRejectsMalformedPins covers the override path: kubeadm
// fails a malformed pin at join time, on the node, where nobody reads it.
func TestBootstrapDiscoveryRejectsMalformedPins(t *testing.T) {
	ctx := context.Background()
	nodeClass := newNodeClass()
	nodeClass.Spec.Bootstrap.APIServerEndpoint = "10.0.0.9:6443"
	nodeClass.Spec.Bootstrap.CACertHashes = []string{"sha256:nothex"}
	h := newHarness(t, nodeClass)
	// Both overrides are set, so cluster-info must not even be consulted.
	h.discovery.refreshErr = context.DeadlineExceeded

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertCondition(t, nc, v1alpha1.ConditionTypeBootstrapDiscoveryReady, metav1.ConditionFalse, "InvalidCACertHash")
}

// TestPlacementGroupCapacity: the eleventh create in a spread placement group
// fails with placement_error, which this provider classifies as a capacity
// error and which is genuinely indistinguishable from a stockout. The warning
// has to come from here or it comes from nowhere. The condition stays True
// throughout, because False would stop the NodePool that is the only thing
// capable of draining the group.
func TestPlacementGroupCapacity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		count      int
		wantReason string
	}{
		{"empty", 0, v1alpha1.ConditionTypePlacementGroupReady},
		{"belowWarn", PlacementGroupWarnServers - 1, v1alpha1.ConditionTypePlacementGroupReady},
		{"atWarn", PlacementGroupWarnServers, "PlacementGroupNearCapacity"},
		{"atCap", PlacementGroupMaxServers, "PlacementGroupAtCapacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t, newNodeClass())
			h.resources.placementGroup = func() (*hcloudapi.PlacementGroup, error) {
				return &hcloudapi.PlacementGroup{ID: 3, Name: "spread", Type: "spread", ServerCount: tc.count}, nil
			}

			nc, err := h.reconcile(ctx)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			assertCondition(t, nc, v1alpha1.ConditionTypePlacementGroupReady, metav1.ConditionTrue, tc.wantReason)
			assertCondition(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
			if nc.Status.PlacementGroup.ServerCount != tc.count {
				t.Errorf("status.placementGroup.serverCount = %d, want %d", nc.Status.PlacementGroup.ServerCount, tc.count)
			}
		})
	}
}

// TestIdleReconcilePerformsNoWrite is the property most easily lost, and it is
// invisible to every other assertion: a redundant write leaves the object
// looking exactly right, bumps its resourceVersion, retriggers the controller's
// own watch, and the result is a hot loop that only shows up as apiserver load.
// So it is asserted by counting writes rather than by inspecting state.
func TestIdleReconcilePerformsNoWrite(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())

	if _, err := h.reconcile(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if got := h.client.patches.Load(); got != 1 {
		t.Errorf("first reconcile made %d metadata patches, want 1 for the finalizer", got)
	}
	if got := h.client.statusPatches.Load(); got != 1 {
		t.Errorf("first reconcile made %d status patches, want exactly 1 for the whole pass", got)
	}

	patchesBefore, statusBefore := h.client.patches.Load(), h.client.statusPatches.Load()
	for i := range 3 {
		if _, err := h.reconcile(ctx); err != nil {
			t.Fatalf("idle reconcile %d: %v", i, err)
		}
	}
	if got := h.client.patches.Load(); got != patchesBefore {
		t.Errorf("idle reconciles made %d metadata patches, want none", got-patchesBefore)
	}
	if got := h.client.statusPatches.Load(); got != statusBefore {
		t.Errorf("idle reconciles made %d status patches, want none", got-statusBefore)
	}
}

// TestTerminationBlocksOnNodeClaims: karpenter core neither cascades nor
// blocks, so without the finalizer a NodeClass can be deleted while live
// NodeClaims reference it, leaving nodes running against a record of how they
// were built that no longer exists.
func TestTerminationBlocksOnNodeClaims(t *testing.T) {
	ctx := context.Background()
	nodeClass := newNodeClass()
	nodeClass.Finalizers = []string{v1alpha1.TerminationFinalizer}
	now := metav1.Now()
	nodeClass.DeletionTimestamp = &now

	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-a"},
		Spec: karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{
			Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: nodeClass.Name,
		}},
	}
	h := newHarness(t, nodeClass, nodeClaim)

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !hasFinalizer(nc) {
		t.Fatal("finalizer was released while a NodeClaim still referenced the NodeClass")
	}
	if got := h.recorder.reasons(); len(got) != 1 || got[0] != "WaitingOnNodeClaimTermination" {
		t.Errorf("events = %v, want one WaitingOnNodeClaimTermination", got)
	}

	// Deleting the last NodeClaim releases the class.
	if err := h.client.Delete(ctx, nodeClaim); err != nil {
		t.Fatalf("delete nodeclaim: %v", err)
	}
	var fetched v1alpha1.HCloudNodeClass
	if err := h.client.Get(ctx, h.name, &fetched); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := h.controller.Reconcile(ctx, &fetched); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	// Releasing the last finalizer on an object that already has a deletion
	// timestamp makes it disappear, in the fake client as at a real apiserver.
	if err := h.client.Get(ctx, h.name, &fetched); !apierrors.IsNotFound(err) {
		t.Errorf("nodeclass still exists after the last NodeClaim went away (get error %v, finalizers %v)", err, fetched.Finalizers)
	}
}

// TestTerminationIgnoresOtherNodeClasses proves the list is scoped by the
// nodeClassRef indexes rather than matching every NodeClaim in the cluster.
func TestTerminationIgnoresOtherNodeClasses(t *testing.T) {
	ctx := context.Background()
	nodeClass := newNodeClass()
	nodeClass.Finalizers = []string{v1alpha1.TerminationFinalizer}
	now := metav1.Now()
	nodeClass.DeletionTimestamp = &now

	other := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-other"},
		Spec: karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{
			Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "some-other-class",
		}},
	}
	h := newHarness(t, nodeClass, other)

	var fetched v1alpha1.HCloudNodeClass
	if err := h.client.Get(ctx, h.name, &fetched); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := h.controller.Reconcile(ctx, &fetched); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(h.recorder.reasons()) != 0 {
		t.Errorf("events = %v, want none: no NodeClaim references this class", h.recorder.reasons())
	}
}

// TestTransientClusterInfoFailureDoesNotUnreadyTheClass.
//
// cluster-info is the one dependency read over the network on every pass rather
// than served from a cache, so a control-plane rolling upgrade or a single
// apiserver replica cycling lands here. Reported as a configuration failure it
// takes every HCloudNodeClass to Ready=False, which core copies onto every
// NodePool as NodeClassReady=False, which stops scheduling and makes
// CloudProvider.Create return NodeClassNotReadyError, at which point core
// DELETES the in-flight NodeClaim. A twenty-second blip must not cost nodes.
func TestTransientClusterInfoFailureDoesNotUnreadyTheClass(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())

	// Resolve once so there is a healthy True condition to preserve.
	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	assertCondition(t, nc, v1alpha1.ConditionTypeBootstrapDiscoveryReady, metav1.ConditionTrue, "")
	endpoint := nc.Status.APIServerEndpoint

	h.discovery.refreshErr = errors.New("etcdserver: request timed out")
	if _, err := h.reconcile(ctx); err == nil {
		t.Fatal("a transient cluster-info failure must be returned as an error so it is retried with backoff")
	}

	if err := h.client.Get(ctx, h.name, nc); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, nc, v1alpha1.ConditionTypeBootstrapDiscoveryReady, metav1.ConditionTrue, "")
	assertCondition(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
	if nc.Status.APIServerEndpoint != endpoint {
		t.Errorf("status.apiServerEndpoint = %q, want it preserved as %q across a blip", nc.Status.APIServerEndpoint, endpoint)
	}
}

// TestEmptyCatalogIsUnknownNotFalse.
//
// A non-nil snapshot carrying no usable location is reachable with no error at
// all, so a single malformed-but-200 response from /v1/server_types would
// otherwise drive every NodeClass in the cluster to Ready=False at once. It is
// a fact about the catalog, not about any NodeClass, so it cannot be False, and
// it must not blame spec.locations either.
func TestEmptyCatalogIsUnknownNotFalse(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())
	h.catalog.snapshot = emptyCatalog().snapshot

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertCondition(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionUnknown, "CatalogEmpty")
	if got := condition(nc, status.ConditionReady); got.Status == metav1.ConditionFalse {
		t.Errorf("Ready = False on an empty catalog; core would stop every NodePool and delete in-flight NodeClaims")
	}
	if len(nc.Status.Locations) != 0 {
		t.Errorf("status.locations = %+v, want cleared", nc.Status.Locations)
	}
}

// TestMissingSSHKeyDoesNotStopProvisioning.
//
// Nothing about joining needs SSH, so a key pruned from the Hetzner project
// after a colleague left must not stop every NodePool that uses the class. The
// loss is real and undetectable from the API afterwards, so it has to be stated
// on the condition and as an event, but it is not a reason to stop.
func TestMissingSSHKeyDoesNotStopProvisioning(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())
	h.resources.sshKey = func(string) (*hcloudapi.SSHKey, error) {
		return nil, &hcloudapi.NotFoundError{Kind: "ssh key", Selector: "ops"}
	}

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertCondition(t, nc, v1alpha1.ConditionTypeSSHKeysReady, metav1.ConditionTrue, "SSHKeysNarrowed")
	assertCondition(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
	if !slices.Contains(h.recorder.reasons(), "SSHKeysNarrowed") {
		t.Errorf("events = %v, want an SSHKeysNarrowed warning; the condition alone is not a notification", h.recorder.reasons())
	}

	// Re-published only on transition. The recorder dedupes for two minutes and
	// this requeues every five, so publishing every pass bills a stable class
	// ~288 warnings a day and trains the operator to ignore them.
	for range 3 {
		if _, err := h.reconcile(ctx); err != nil {
			t.Fatalf("repeat reconcile: %v", err)
		}
	}
	if got := slices.Compact(h.recorder.reasons()); len(got) != 1 {
		t.Errorf("events = %v, want the warning published once", h.recorder.reasons())
	}
}

// TestValidationNamesTheFailingDependency: core copies this reason and message
// verbatim onto the NodePool's NodeClassReady, where a fixed "awaiting
// resolution" of six things the operator cannot see from there is not a
// diagnosis.
func TestValidationNamesTheFailingDependency(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())
	h.resources.network = func() (*hcloudapi.Network, error) {
		return nil, &hcloudapi.NotFoundError{Kind: "network", Selector: "cluster"}
	}

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := condition(nc, v1alpha1.ConditionTypeValidationSucceeded)
	if !strings.Contains(got.Message, v1alpha1.ConditionTypeNetworkReady) {
		t.Errorf("ValidationSucceeded message = %q, want it to name %s", got.Message, v1alpha1.ConditionTypeNetworkReady)
	}
	if strings.Contains(got.Message, v1alpha1.ConditionTypeImageReady) {
		t.Errorf("ValidationSucceeded message = %q, names a dependency that resolved fine", got.Message)
	}
}

// TestRejectedCredentialNamesTheCredential: without this, a token rotation
// produces up to five simultaneous conditions that each blame a different
// selector, every one of which is correct, with nothing anywhere naming the
// token.
func TestRejectedCredentialNamesTheCredential(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newNodeClass())
	h.resources.image = func() (*hcloudapi.Image, error) { return nil, fatalErr }

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := condition(nc, v1alpha1.ConditionTypeImageReady)
	if !strings.Contains(got.Message, "credential") {
		t.Errorf("ImageReady message = %q, want it to name the credential rather than the selector", got.Message)
	}
}

// TestDuplicateLocationsAreDeduplicated: the CRD constrains the length and the
// shape of each entry but not uniqueness, and a repeat would become a duplicate
// status entry and, downstream, a duplicate offering.
func TestDuplicateLocationsAreDeduplicated(t *testing.T) {
	ctx := context.Background()
	nodeClass := newNodeClass()
	nodeClass.Spec.Locations = []string{"nbg1", "fsn1", "nbg1"}
	h := newHarness(t, nodeClass)

	nc, err := h.reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(nc.Status.Locations) != 2 {
		t.Errorf("status.locations = %+v, want nbg1 and fsn1 once each", nc.Status.Locations)
	}
}
