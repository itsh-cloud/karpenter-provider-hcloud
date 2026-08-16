package hash

import (
	"context"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

const oldHashVersion = "v0"

func newFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.HCloudNodeClass{}, &karpv1.NodeClaim{}).
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

func newNodeClass(annotations map[string]string) *v1alpha1.HCloudNodeClass {
	return &v1alpha1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Annotations: annotations},
		Spec: v1alpha1.HCloudNodeClassSpec{
			ImageSelector: v1alpha1.ImageSelectorTerm{Name: "debian-13"},
			Bootstrap:     v1alpha1.BootstrapSpec{KubernetesVersion: "1.34.7"},
		},
	}
}

func newNodeClaim(name string, annotations map[string]string, drifted bool) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec: karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{
			Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default",
		}},
	}
	if drifted {
		nc.Status.Conditions = []status.Condition{{
			Type:               karpv1.ConditionTypeDrifted,
			Status:             metav1.ConditionTrue,
			Reason:             "NodeClassDrifted",
			LastTransitionTime: metav1.Now(),
		}}
	}
	return nc
}

func runReconcile(t *testing.T, c client.Client) *v1alpha1.HCloudNodeClass {
	t.Helper()
	ctx := context.Background()
	var nc v1alpha1.HCloudNodeClass
	if err := c.Get(ctx, types.NamespacedName{Name: "default"}, &nc); err != nil {
		t.Fatalf("get nodeclass: %v", err)
	}
	if _, err := NewController(c).Reconcile(ctx, &nc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var out v1alpha1.HCloudNodeClass
	if err := c.Get(ctx, types.NamespacedName{Name: "default"}, &out); err != nil {
		t.Fatalf("get nodeclass after reconcile: %v", err)
	}
	return &out
}

func getNodeClaim(t *testing.T, c client.Client, name string) *karpv1.NodeClaim {
	t.Helper()
	var nc karpv1.NodeClaim
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &nc); err != nil {
		t.Fatalf("get nodeclaim %s: %v", name, err)
	}
	return &nc
}

func TestStampsHashAndVersion(t *testing.T) {
	nodeClass := newNodeClass(nil)
	c := newFakeClient(nodeClass)

	got := runReconcile(t, c)
	if got.Annotations[v1alpha1.AnnotationHash] != nodeClass.Hash() {
		t.Errorf("hash annotation = %q, want %q", got.Annotations[v1alpha1.AnnotationHash], nodeClass.Hash())
	}
	if got.Annotations[v1alpha1.AnnotationHashVersion] != v1alpha1.HashVersion {
		t.Errorf("hash version annotation = %q, want %q", got.Annotations[v1alpha1.AnnotationHashVersion], v1alpha1.HashVersion)
	}
}

// TestIdleReconcileDoesNotWrite: this controller runs on every NodeClass event,
// and a patch on every pass would retrigger its own watch.
func TestIdleReconcileDoesNotWrite(t *testing.T) {
	nodeClass := newNodeClass(nil)
	c := newFakeClient(nodeClass)

	first := runReconcile(t, c)
	second := runReconcile(t, c)
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("resourceVersion changed on an idle reconcile: %s then %s", first.ResourceVersion, second.ResourceVersion)
	}
}

// TestHashVersionBumpDoesNotDriftTheFleet is the whole reason the version
// annotation exists.
//
// Changing the hash generator changes the hash of an unchanged spec. If the
// NodeClass were simply re-stamped, every NodeClaim would be holding a hash
// produced by the old generator while the class held one produced by the new,
// and the drift comparator would read that as "every node in the fleet is out
// of date" and replace all of them at once. The back-fill gives each NodeClaim
// the NEW hash of the SAME class first, so the pair agrees again, and only then
// advances the class's own version.
//
// The property is asserted in exactly the terms the comparator uses: equal
// versions on both sides, and equal hashes on both sides, which is what makes
// it report "not drifted".
func TestHashVersionBumpDoesNotDriftTheFleet(t *testing.T) {
	nodeClass := newNodeClass(map[string]string{
		v1alpha1.AnnotationHash:        "hash-from-the-old-generator",
		v1alpha1.AnnotationHashVersion: oldHashVersion,
	})
	// Two live nodes carrying the old generator's output, which is what an
	// upgrade actually finds.
	claimA := newNodeClaim("claim-a", map[string]string{
		v1alpha1.AnnotationHash:        "hash-from-the-old-generator",
		v1alpha1.AnnotationHashVersion: oldHashVersion,
	}, false)
	claimB := newNodeClaim("claim-b", map[string]string{
		v1alpha1.AnnotationHash:        "some-other-old-hash",
		v1alpha1.AnnotationHashVersion: oldHashVersion,
	}, false)
	c := newFakeClient(nodeClass, claimA, claimB)

	got := runReconcile(t, c)

	for _, name := range []string{"claim-a", "claim-b"} {
		claim := getNodeClaim(t, c, name)
		if claim.Annotations[v1alpha1.AnnotationHashVersion] != got.Annotations[v1alpha1.AnnotationHashVersion] {
			t.Errorf("%s: hash version = %q, nodeclass = %q; a mismatch here means drift silently stops being evaluated",
				name, claim.Annotations[v1alpha1.AnnotationHashVersion], got.Annotations[v1alpha1.AnnotationHashVersion])
		}
		if claim.Annotations[v1alpha1.AnnotationHash] != got.Annotations[v1alpha1.AnnotationHash] {
			t.Errorf("%s: hash = %q, nodeclass = %q; unequal at equal versions is exactly the mass drift this must prevent",
				name, claim.Annotations[v1alpha1.AnnotationHash], got.Annotations[v1alpha1.AnnotationHash])
		}
	}
}

// TestAlreadyDriftedNodeClaimKeepsItsStaleHash: a NodeClaim that is already
// marked Drifted is condemned and scheduled for replacement. Re-stamping it
// would un-drift it, and since its stored hash came from the old generator
// there is no way to recompute whether the reason it drifted still holds.
// Cancelling a replacement is the unrecoverable direction to guess wrong in.
func TestAlreadyDriftedNodeClaimKeepsItsStaleHash(t *testing.T) {
	nodeClass := newNodeClass(map[string]string{
		v1alpha1.AnnotationHash:        "hash-from-the-old-generator",
		v1alpha1.AnnotationHashVersion: oldHashVersion,
	})
	drifted := newNodeClaim("claim-drifted", map[string]string{
		v1alpha1.AnnotationHash:        "stale-hash",
		v1alpha1.AnnotationHashVersion: oldHashVersion,
	}, true)
	c := newFakeClient(nodeClass, drifted)

	got := runReconcile(t, c)
	claim := getNodeClaim(t, c, "claim-drifted")

	if claim.Annotations[v1alpha1.AnnotationHashVersion] != v1alpha1.HashVersion {
		t.Errorf("hash version = %q, want %q: the version advances even for a drifted claim",
			claim.Annotations[v1alpha1.AnnotationHashVersion], v1alpha1.HashVersion)
	}
	if claim.Annotations[v1alpha1.AnnotationHash] != "stale-hash" {
		t.Errorf("hash = %q, want the stale one retained", claim.Annotations[v1alpha1.AnnotationHash])
	}
	if claim.Annotations[v1alpha1.AnnotationHash] == got.Annotations[v1alpha1.AnnotationHash] {
		t.Error("a drifted claim was un-drifted by the back-fill")
	}
	// Inspecting the Drifted condition must not have fabricated the rest of the
	// NodeClaim's condition set onto the stored object.
	if len(claim.Status.Conditions) != 1 {
		t.Errorf("nodeclaim conditions = %d, want the single Drifted condition it started with", len(claim.Status.Conditions))
	}
}

// TestBackfillSkipsUpToDateNodeClaims: a NodeClaim launched after the upgrade
// already carries the current version, and rewriting its hash would clobber the
// value stamped at launch.
func TestBackfillSkipsUpToDateNodeClaims(t *testing.T) {
	nodeClass := newNodeClass(map[string]string{
		v1alpha1.AnnotationHash:        "hash-from-the-old-generator",
		v1alpha1.AnnotationHashVersion: oldHashVersion,
	})
	current := newNodeClaim("claim-current", map[string]string{
		v1alpha1.AnnotationHash:        "launched-with-this-hash",
		v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion,
	}, false)
	c := newFakeClient(nodeClass, current)

	runReconcile(t, c)
	claim := getNodeClaim(t, c, "claim-current")
	if claim.Annotations[v1alpha1.AnnotationHash] != "launched-with-this-hash" {
		t.Errorf("hash = %q, want the launch-time value left alone", claim.Annotations[v1alpha1.AnnotationHash])
	}
}

// TestBackfillIgnoresOtherNodeClasses proves the list is scoped by the
// nodeClassRef indexes rather than sweeping every NodeClaim in the cluster.
func TestBackfillIgnoresOtherNodeClasses(t *testing.T) {
	nodeClass := newNodeClass(map[string]string{
		v1alpha1.AnnotationHash:        "hash-from-the-old-generator",
		v1alpha1.AnnotationHashVersion: oldHashVersion,
	})
	other := newNodeClaim("claim-other", map[string]string{
		v1alpha1.AnnotationHash:        "someone-elses-hash",
		v1alpha1.AnnotationHashVersion: oldHashVersion,
	}, false)
	other.Spec.NodeClassRef.Name = "another-class"
	c := newFakeClient(nodeClass, other)

	runReconcile(t, c)
	claim := getNodeClaim(t, c, "claim-other")
	if claim.Annotations[v1alpha1.AnnotationHashVersion] != oldHashVersion {
		t.Errorf("hash version = %q, want it untouched: this claim belongs to a different NodeClass",
			claim.Annotations[v1alpha1.AnnotationHashVersion])
	}
}

// TestTerminatingNodeClassIsNotStamped: a class being deleted will never launch
// another node, so writing its hash only races the finalizer.
func TestTerminatingNodeClassIsNotStamped(t *testing.T) {
	nodeClass := newNodeClass(nil)
	nodeClass.Finalizers = []string{v1alpha1.TerminationFinalizer}
	now := metav1.Now()
	nodeClass.DeletionTimestamp = &now
	c := newFakeClient(nodeClass)

	got := runReconcile(t, c)
	if _, ok := got.Annotations[v1alpha1.AnnotationHash]; ok {
		t.Errorf("annotations = %v, want no hash written to a terminating class", got.Annotations)
	}
}
