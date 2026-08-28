package cloudprovider

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/catalog"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instance"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instancetype"
)

const testCluster = "test-cluster"

type fakeCatalog struct{ snapshot *catalog.Snapshot }

func (f *fakeCatalog) Get() *catalog.Snapshot { return f.snapshot }

type fakeBootstrapper struct {
	rendered int
	err      error
}

func (f *fakeBootstrapper) Render(context.Context, *v1alpha1.HCloudNodeClass, *karpv1.NodeClaim) (string, error) {
	f.rendered++
	return "#cloud-config", f.err
}

// fakeServers is the minimum needed to reach the CloudProvider's own logic.
var errTransient = errors.New("dial tcp: i/o timeout")

type fakeServers struct {
	byID    map[int64]*hcloudapi.Server
	created []hcloudapi.CreateServerRequest
	getErr  error
}

func (f *fakeServers) Create(_ context.Context, req hcloudapi.CreateServerRequest) (*hcloudapi.Server, error) {
	f.created = append(f.created, req)
	srv := &hcloudapi.Server{ID: 1, Name: req.Name, ServerType: req.ServerType, Location: req.Location, Labels: req.Labels}
	if f.byID == nil {
		f.byID = map[int64]*hcloudapi.Server{}
	}
	f.byID[1] = srv
	return srv, nil
}
func (f *fakeServers) Delete(_ context.Context, id int64) error {
	if _, ok := f.byID[id]; !ok {
		return &hcloudapi.NotFoundError{Kind: "server", Selector: "x"}
	}
	delete(f.byID, id)
	return nil
}
func (f *fakeServers) Get(_ context.Context, id int64) (*hcloudapi.Server, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.byID[id], nil
}
func (f *fakeServers) GetByName(context.Context, string) (*hcloudapi.Server, error) { return nil, nil }
func (f *fakeServers) List(context.Context, string) ([]*hcloudapi.Server, error) {
	out := make([]*hcloudapi.Server, 0, len(f.byID))
	for _, s := range f.byID {
		out = append(out, s)
	}
	return out, nil
}

func readyNodeClass() *v1alpha1.HCloudNodeClass {
	nc := &v1alpha1.HCloudNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nc.Status.Image = &v1alpha1.ImageStatus{ID: 42, Name: "debian-13"}
	nc.Status.APIServerEndpoint = "10.1.0.2:6443"
	// Mark every dependent condition True so the roll-up reads Ready.
	conds := nc.StatusConditions()
	for _, c := range nc.StatusConditions().List() {
		conds.SetTrue(c.Type)
	}
	return nc
}

func notReadyNodeClass() *v1alpha1.HCloudNodeClass {
	nc := &v1alpha1.HCloudNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nc.Status.Image = &v1alpha1.ImageStatus{ID: 42}
	nc.StatusConditions().SetFalse(v1alpha1.ConditionTypeImageReady, "ImageNotFound", "no such image")
	return nc
}

func newTestProvider(t *testing.T, objs ...client.Object) (*CloudProvider, *fakeServers) {
	t.Helper()
	kubeClient := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.HCloudNodeClass{}).
		Build()

	servers := &fakeServers{byID: map[int64]*hcloudapi.Server{}}
	unavailable := instancetype.NewUnavailable()
	instances := instance.NewProvider(servers, unavailable, testCluster)
	snapshot := &catalog.Snapshot{ServerTypes: []hcloudapi.ServerType{{
		Name: "cx33", Architecture: "x86", Cores: 4, MemoryGiB: 8, DiskGB: 80,
		Prices:    map[string]hcloudapi.Price{"nbg1": {MonthlyNet: 8.49}},
		Locations: []hcloudapi.ServerTypeLocation{{Location: "nbg1", NetworkZone: "eu-central", Available: true}},
	}}}
	return New(kubeClient, instances, &fakeCatalog{snapshot: snapshot}, unavailable, &fakeBootstrapper{}, testCluster), servers
}

// TestCreateRefusesAnUnreadyNodeClass.
//
// Every id a create uses comes from the NodeClass STATUS, so launching against
// a class whose status has not resolved would order a server against stale or
// absent ids. Reported as NodeClassNotReady so core surfaces it on the NodePool
// rather than failing the NodeClaim outright.
func TestCreateRefusesAnUnreadyNodeClass(t *testing.T) {
	nc := notReadyNodeClass()
	cp, servers := newTestProvider(t, nc)

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default"},
		},
	}

	_, err := cp.Create(context.Background(), claim)
	if err == nil {
		t.Fatal("launched against a NodeClass that is not ready")
	}
	if !cloudprovider.IsNodeClassNotReadyError(err) {
		t.Errorf("err = %v, want a NodeClassNotReadyError", err)
	}
	if len(servers.created) != 0 {
		t.Error("a server was ordered despite the NodeClass being unready")
	}
}

// TestCreateRefusesATerminatingNodeClass: the finalizer is holding it open
// precisely because nodes still reference it, and adding another is the wrong
// direction.
func TestCreateRefusesATerminatingNodeClass(t *testing.T) {
	nc := readyNodeClass()
	now := metav1.Now()
	nc.DeletionTimestamp = &now
	nc.Finalizers = []string{v1alpha1.TerminationFinalizer}
	cp, servers := newTestProvider(t, nc)

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default"},
		},
	}
	if _, err := cp.Create(context.Background(), claim); err == nil {
		t.Fatal("launched against a terminating NodeClass")
	}
	if len(servers.created) != 0 {
		t.Error("a server was ordered against a terminating NodeClass")
	}
}

// TestDeleteReportsNotFoundSoCoreStops.
//
// Karpenter retries Delete until it reports NodeClaimNotFound. Returning a
// plain error for an already-gone server spins forever and the NodeClaim never
// finishes terminating.
func TestDeleteReportsNotFoundSoCoreStops(t *testing.T) {
	cp, _ := newTestProvider(t)

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status:     karpv1.NodeClaimStatus{ProviderID: "hcloud://424242"},
	}
	err := cp.Delete(context.Background(), claim)
	if err == nil {
		t.Fatal("expected not-found rather than success")
	}
	if !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("err = %v, want NodeClaimNotFoundError so core stops retrying", err)
	}
}

// TestDeleteWithoutProviderIDIsNotFound: a NodeClaim that never launched has
// nothing to remove, and core must be allowed to finish terminating it.
func TestDeleteWithoutProviderIDIsNotFound(t *testing.T) {
	cp, _ := newTestProvider(t)
	err := cp.Delete(context.Background(), &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "n1"}})
	if !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("err = %v, want NodeClaimNotFoundError", err)
	}
}

// TestListOnlyReturnsOwnedServers is what karpenter's garbage collector reads.
// A server wrongly included here is one core may adopt and later delete.
func TestListOnlyReturnsOwnedServers(t *testing.T) {
	cp, servers := newTestProvider(t)
	servers.byID[1] = &hcloudapi.Server{ID: 1, Name: "worker", ServerType: "cx33", Location: "nbg1",
		Labels: map[string]string{hcloudapi.LabelManagedBy: testCluster}}

	claims, err := cp.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(claims) != 1 || claims[0].Status.ProviderID != "hcloud://1" {
		t.Fatalf("List = %+v, want one claim for hcloud://1", claims)
	}
	if got := claims[0].Labels[corev1.LabelInstanceTypeStable]; got != "cx33" {
		t.Errorf("instance-type label = %q, want cx33", got)
	}
	// Capacity is read by core's cost accounting and its garbage collector.
	if claims[0].Status.Capacity.Cpu().Value() != 4 {
		t.Errorf("capacity cpu = %v, want 4 from the catalog", claims[0].Status.Capacity.Cpu())
	}
}

// TestGetIsNotFoundForUnownedServers: everything downstream treats a returned
// NodeClaim as a node karpenter may manage.
func TestGetIsNotFoundForUnownedServers(t *testing.T) {
	cp, servers := newTestProvider(t)
	servers.byID[9] = &hcloudapi.Server{ID: 9, Name: "k8s-node-1"} // not ours, unlabelled

	_, err := cp.Get(context.Background(), "hcloud://9")
	if !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("err = %v, want NodeClaimNotFoundError for a server we do not own", err)
	}
}

// TestGetSupportedNodeClassesReturnsAFreshObject.
//
// karpenter's pkg/utils/nodepool does client.Get INTO this object and the
// readiness controller hands the same one to builder.Watches, so a shared
// value would be mutated concurrently by every NodePool reconcile.
func TestGetSupportedNodeClassesReturnsAFreshObject(t *testing.T) {
	cp, _ := newTestProvider(t)
	a, b := cp.GetSupportedNodeClasses(), cp.GetSupportedNodeClasses()
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("want exactly one supported NodeClass, got %d and %d", len(a), len(b))
	}
	if a[0] == b[0] {
		t.Error("two calls returned the SAME pointer; concurrent reconciles would share it")
	}
}

// TestInstanceTypesRefuseAnEmptyCatalog: returning an empty list tells
// karpenter the cluster can hold nothing, which presents as every pod being
// permanently unschedulable rather than as a transient outage.
func TestInstanceTypesRefuseAnEmptyCatalog(t *testing.T) {
	cp, _ := newTestProvider(t)
	cp.catalog = &fakeCatalog{snapshot: nil}

	_, err := cp.instanceTypes(readyNodeClass())
	if err == nil {
		t.Fatal("returned instance types from a catalog that has never been fetched")
	}
}

func TestNameIsStable(t *testing.T) {
	cp, _ := newTestProvider(t)
	if cp.Name() != "hcloud" {
		t.Errorf("Name() = %q; it appears in karpenter's metrics and must not drift", cp.Name())
	}
}

// TestCapacityIsConsistentBetweenCreateAndList.
//
// Create publishes Status.Capacity from the instance type, which has the VM
// overhead correction applied. Get and List take a different route, so without
// deliberate care the same server reports roughly 7% more memory through one
// than the other, and core's cost accounting and its garbage collector both
// read this.
func TestCapacityIsConsistentBetweenCreateAndList(t *testing.T) {
	cp, servers := newTestProvider(t)
	servers.byID[1] = &hcloudapi.Server{
		ID: 1, Name: "worker", ServerType: "cx33", Location: "nbg1",
		Labels: map[string]string{hcloudapi.LabelManagedBy: testCluster},
	}

	claims, err := cp.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	listed := claims[0].Status.Capacity

	// What Create would publish for the same type, via the instance type.
	types, err := cp.instanceTypes(readyNodeClass())
	if err != nil {
		t.Fatalf("instanceTypes: %v", err)
	}
	var created corev1.ResourceList
	for _, it := range types {
		if it.Name == "cx33" {
			created = it.Capacity
		}
	}
	if created == nil {
		t.Fatal("cx33 missing from the built instance types")
	}

	for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage, corev1.ResourcePods} {
		want, got := created[r], listed[r]
		if want.Cmp(got) != 0 {
			t.Errorf("%s: Create publishes %s, List publishes %s; the same server must report one capacity", r, want.String(), got.String())
		}
	}
}

// TestListReportsCorrectedMemoryNotAdvertised pins the direction, so the test
// above cannot be satisfied by making both sides equally wrong.
func TestListReportsCorrectedMemoryNotAdvertised(t *testing.T) {
	cp, servers := newTestProvider(t)
	servers.byID[1] = &hcloudapi.Server{
		ID: 1, Name: "worker", ServerType: "cx33", Location: "nbg1",
		Labels: map[string]string{hcloudapi.LabelManagedBy: testCluster},
	}

	claims, err := cp.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mem := claims[0].Status.Capacity.Memory().Value()
	advertised := int64(8) * 1024 * 1024 * 1024
	if mem >= advertised {
		t.Errorf("memory = %d, want less than the advertised %d; a machine never reports its full advertised RAM", mem, advertised)
	}
	if mem < advertised*90/100 {
		t.Errorf("memory = %d, more than 10%% below advertised %d; the correction is too aggressive", mem, advertised)
	}
}

// TestHashIsStampedOnLaunch.
//
// Without this the whole hash half of drift is dead code in production while
// its unit tests stay green: the hash controller only writes this annotation
// during a version back-fill, which runs once and never again, so no NodeClaim
// launched afterwards carries one and drift declines forever. Everything the
// hash covers, user data, ssh keys, kubelet config, would silently never drift.
func TestHashIsStampedOnLaunch(t *testing.T) {
	nc := readyNodeClass()
	cp, _ := newTestProvider(t, nc)

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default"},
		},
	}
	out, err := cp.Create(context.Background(), claim)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := out.Annotations[v1alpha1.AnnotationHashVersion]; got != v1alpha1.HashVersion {
		t.Errorf("hash version annotation = %q, want %q", got, v1alpha1.HashVersion)
	}
	if got := out.Annotations[v1alpha1.AnnotationHash]; got != nc.Hash() {
		t.Errorf("hash annotation = %q, want the NodeClass's %q", got, nc.Hash())
	}
}

// TestLaunchedNodeClaimCarriesTheZoneTheCCMWillWrite.
//
// The two must agree or karpenter prices the node at 0 and never replaces it.
// They agree by construction, not by luck: the CCM derives the label from the
// location with a pure function and never reads the server's real datacenter,
// so this asserts the shared answer rather than a guess.
func TestLaunchedNodeClaimCarriesTheZoneTheCCMWillWrite(t *testing.T) {
	nc := readyNodeClass()
	cp, _ := newTestProvider(t, nc)

	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default"},
		},
	}
	out, err := cp.Create(context.Background(), claim)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := out.Labels[corev1.LabelTopologyZone]; got != "nbg1-dc3" {
		t.Errorf("zone label = %q, want nbg1-dc3, which is what hcloud-CCM writes for a server in nbg1", got)
	}
	if got := out.Labels[corev1.LabelTopologyRegion]; got != "nbg1" {
		t.Errorf("region label = %q, want nbg1", got)
	}
}
