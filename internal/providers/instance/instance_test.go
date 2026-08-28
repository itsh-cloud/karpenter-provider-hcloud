package instance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	coresched "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instancetype"
)

const testCluster = "test-cluster"

// fakeServers records every create and can be scripted to fail per attempt.
type fakeServers struct {
	mu       sync.Mutex
	attempts []hcloudapi.CreateServerRequest
	fail     func(n int, req hcloudapi.CreateServerRequest) error
	byName   map[string]*hcloudapi.Server
	byID     map[int64]*hcloudapi.Server
	listed   []*hcloudapi.Server
	listErr  error
	listCall int
	getCall  int
	deleted  []int64
	nextID   int64
}

func (f *fakeServers) getCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCall
}

func newFakeServers() *fakeServers {
	return &fakeServers{byName: map[string]*hcloudapi.Server{}, byID: map[int64]*hcloudapi.Server{}, nextID: 1000}
}

func (f *fakeServers) Create(_ context.Context, req hcloudapi.CreateServerRequest) (*hcloudapi.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.attempts)
	f.attempts = append(f.attempts, req)
	if f.fail != nil {
		if err := f.fail(n, req); err != nil {
			return nil, err
		}
	}
	f.nextID++
	srv := &hcloudapi.Server{
		ID: f.nextID, Name: req.Name, ServerType: req.ServerType,
		Location: req.Location, Labels: req.Labels, Status: "running",
	}
	f.byName[req.Name] = srv
	f.byID[srv.ID] = srv
	return srv, nil
}

func (f *fakeServers) Delete(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	if _, ok := f.byID[id]; !ok {
		return &hcloudapi.NotFoundError{Kind: "server", Selector: fmt.Sprint(id)}
	}
	delete(f.byID, id)
	return nil
}

func (f *fakeServers) Get(_ context.Context, id int64) (*hcloudapi.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCall++
	return f.byID[id], nil
}

func (f *fakeServers) GetByName(_ context.Context, name string) (*hcloudapi.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byName[name], nil
}

func (f *fakeServers) List(_ context.Context, _ string) ([]*hcloudapi.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCall++
	return f.listed, f.listErr
}

func (f *fakeServers) serverTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.attempts))
	for _, a := range f.attempts {
		out = append(out, a.ServerType+"@"+a.Location)
	}
	return out
}

func capacityErr(code hcloud.ErrorCode) error {
	return hcloud.Error{Code: code, Message: string(code)}
}

// instanceType builds a type offered in each location at the given monthly
// price, which the offering divides into an hourly one.
func instanceType(name string, cpu int64, memGiB int64, price float64, locations ...string) *cloudprovider.InstanceType {
	var offerings cloudprovider.Offerings
	for _, l := range locations {
		offerings = append(offerings, &cloudprovider.Offering{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, l),
			),
			Price:     price,
			Available: true,
		})
	}
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(cpu, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(memGiB*1024*1024*1024, resource.BinarySI),
	}
	return &cloudprovider.InstanceType{
		Name:      name,
		Capacity:  capacity,
		Offerings: offerings,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, locations...),
		),
		Overhead: &cloudprovider.InstanceTypeOverhead{},
	}
}

func testNodeClass() *v1alpha1.HCloudNodeClass {
	nc := &v1alpha1.HCloudNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nc.Status.Image = &v1alpha1.ImageStatus{ID: 42, Name: "debian-13"}
	nc.Status.Network = &v1alpha1.NetworkStatus{ID: 7, Name: "k8s-network", Zone: "eu-central"}
	return nc
}

func testNodeClaim() *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "autoscaled-general-nbg1-abcde",
			Labels: map[string]string{karpv1.NodePoolLabelKey: "autoscaled-general-nbg1"},
		},
	}
}

// TestCreateOrdersByPriceNotByInputOrder.
//
// The founding defect of this project. Karpenter core computes a price order
// and then loses it: it serialises the result into a requirement whose values
// come from a Go map, so any provider that walks the input order picks
// effectively at random and the fleet converges on whatever came out first.
// The provider must re-derive the ordering from its own catalog.
func TestCreateOrdersByPriceNotByInputOrder(t *testing.T) {
	f := newFakeServers()
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	// The names deliberately sort in the OPPOSITE order to the prices.
	//
	// With names that happen to ascend with price, a provider that does not
	// sort by price at all still lands on the cheapest, via the alphabetical
	// tiebreak or plain input order, and the test passes while proving
	// nothing. Only a genuine price comparison picks "aaa-expensive" last.
	types := []*cloudprovider.InstanceType{
		instanceType("aaa-expensive", 16, 32, 29.49, "nbg1"),
		instanceType("mmm-middling", 8, 16, 15.99, "nbg1"),
		instanceType("zzz-cheapest", 4, 8, 8.49, "nbg1"),
	}

	srv, it, err := p.Create(context.Background(), testNodeClass(), testNodeClaim(), types, "#cloud-config")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if it.Name != "zzz-cheapest" || srv.ServerType != "zzz-cheapest" {
		t.Errorf("ordered %v, took %q; want the cheapest, zzz-cheapest", f.serverTypes(), srv.ServerType)
	}
}

// TestCandidateOrderIsDeterministic: equally priced candidates must not swap
// between passes, or a replacement picks a different type each time and the
// fleet never settles.
func TestCandidateOrderIsDeterministic(t *testing.T) {
	p := NewProvider(newFakeServers(), instancetype.NewUnavailable(), testCluster)
	types := []*cloudprovider.InstanceType{
		instanceType("cx43", 8, 16, 15.99, "fsn1", "nbg1"),
		instanceType("cpx31", 8, 16, 15.99, "nbg1", "fsn1"),
	}

	var first []string
	for range 10 {
		var got []string
		for _, c := range p.orderedCandidates(testNodeClaim(), types) {
			got = append(got, c.InstanceType.Name+"@"+c.Location)
		}
		if first == nil {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("candidate count changed between passes: %v vs %v", first, got)
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("candidate order is not deterministic: %v then %v", first, got)
			}
		}
	}
}

// TestCreateFallsThroughOnCapacityErrors.
//
// The second founding defect: falling through on ONLY resource_unavailable
// leaves placement_error and no_space_left_in_location stranding the NodeClaim,
// even though all three mean "not here, not now".
func TestCreateFallsThroughOnCapacityErrors(t *testing.T) {
	for _, code := range []hcloud.ErrorCode{
		hcloud.ErrorCodeResourceUnavailable,
		hcloud.ErrorCodePlacementError,
		hcloud.ErrorCodeNoSpaceLeftInLocation,
	} {
		t.Run(string(code), func(t *testing.T) {
			f := newFakeServers()
			// The two cheapest are out of stock; the third must be taken.
			f.fail = func(n int, _ hcloudapi.CreateServerRequest) error {
				if n < 2 {
					return capacityErr(code)
				}
				return nil
			}
			unavailable := instancetype.NewUnavailable()
			p := NewProvider(f, unavailable, testCluster)

			types := []*cloudprovider.InstanceType{
				instanceType("cx33", 4, 8, 8.49, "nbg1"),
				instanceType("cx43", 8, 16, 15.99, "nbg1"),
				instanceType("cx53", 16, 32, 29.49, "nbg1"),
			}

			srv, _, err := p.Create(context.Background(), testNodeClass(), testNodeClaim(), types, "#cloud-config")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if srv.ServerType != "cx53" {
				t.Errorf("attempts %v, landed on %q; want the fall-through to reach cx53", f.serverTypes(), srv.ServerType)
			}
			// The failures must be remembered, or the next scheduling pass
			// re-attempts the same doomed pair immediately.
			if !unavailable.Has("cx33", "nbg1") || !unavailable.Has("cx43", "nbg1") {
				t.Error("the failed (type, location) pairs were not marked unavailable")
			}
		})
	}
}

// TestCreateDoesNotMarkUnavailableOnRateLimit.
//
// Rate limiting is caused by our own request volume. Treating it as a capacity
// signal poisons the catalog, so a slowdown becomes a self-inflicted capacity
// outage that outlives the throttling.
func TestCreateDoesNotMarkUnavailableOnRateLimit(t *testing.T) {
	f := newFakeServers()
	f.fail = func(int, hcloudapi.CreateServerRequest) error {
		return capacityErr(hcloud.ErrorCodeRateLimitExceeded)
	}
	unavailable := instancetype.NewUnavailable()
	p := NewProvider(f, unavailable, testCluster)

	types := []*cloudprovider.InstanceType{instanceType("cx33", 4, 8, 8.49, "nbg1")}
	if _, _, err := p.Create(context.Background(), testNodeClass(), testNodeClaim(), types, "#cloud-config"); err == nil {
		t.Fatal("expected a rate limit to be returned as an error")
	}
	if unavailable.Has("cx33", "nbg1") {
		t.Error("a rate limit marked the offering unavailable; that converts throttling into a capacity outage")
	}
	if got := len(f.attempts); got != 1 {
		t.Errorf("made %d attempts on a rate limit, want 1; retrying immediately makes the throttling worse", got)
	}
}

// TestCreateStampsOwnershipLabels: without these a server is invisible to
// every cleanup path and belongs to nobody.
func TestCreateStampsOwnershipLabels(t *testing.T) {
	f := newFakeServers()
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	types := []*cloudprovider.InstanceType{instanceType("cx33", 4, 8, 8.49, "nbg1")}
	srv, _, err := p.Create(context.Background(), testNodeClass(), testNodeClaim(), types, "#cloud-config")
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.Labels[hcloudapi.LabelManagedBy]; got != testCluster {
		t.Errorf("%s = %q, want %q", hcloudapi.LabelManagedBy, got, testCluster)
	}
	if got := srv.Labels[hcloudapi.LabelNodeClaim]; got != "autoscaled-general-nbg1-abcde" {
		t.Errorf("%s = %q", hcloudapi.LabelNodeClaim, got)
	}
	if !srv.IsManagedBy(testCluster) {
		t.Error("the server this provider just created does not read as managed by it")
	}
}

// TestServerLabelsCannotOverrideOwnership: a NodeClass is operator-authored
// config, and letting it set the ownership label would let it hide its servers
// from cleanup, or claim another cluster's.
func TestServerLabelsCannotOverrideOwnership(t *testing.T) {
	f := newFakeServers()
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	nc := testNodeClass()
	nc.Spec.ServerLabels = map[string]string{
		hcloudapi.LabelManagedBy: "someone-elses-cluster",
		hcloudapi.LabelNodeClaim: "not-this-one",
		"cost-center":            "platform",
	}

	types := []*cloudprovider.InstanceType{instanceType("cx33", 4, 8, 8.49, "nbg1")}
	srv, _, err := p.Create(context.Background(), nc, testNodeClaim(), types, "#cloud-config")
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.Labels[hcloudapi.LabelManagedBy]; got != testCluster {
		t.Errorf("%s = %q; a NodeClass overrode the ownership label", hcloudapi.LabelManagedBy, got)
	}
	if got := srv.Labels["cost-center"]; got != "platform" {
		t.Errorf("cost-center = %q; ordinary operator labels must still apply", got)
	}
}

// TestCreateRefusesWithoutClusterName: an unowned server is one no cleanup path
// will ever remove, so it must never be created in the first place.
func TestCreateRefusesWithoutClusterName(t *testing.T) {
	f := newFakeServers()
	p := NewProvider(f, instancetype.NewUnavailable(), "")

	types := []*cloudprovider.InstanceType{instanceType("cx33", 4, 8, 8.49, "nbg1")}
	if _, _, err := p.Create(context.Background(), testNodeClass(), testNodeClaim(), types, "#cloud-config"); err == nil {
		t.Fatal("expected a refusal to create an unowned server")
	}
	if len(f.attempts) != 0 {
		t.Error("a server was ordered despite having no owner")
	}
}

// TestCreateAdoptsOnUniquenessError: the create succeeded but its response was
// lost. Failing here would leak a running, billing server nothing owns.
func TestCreateAdoptsOnUniquenessError(t *testing.T) {
	f := newFakeServers()
	existing := &hcloudapi.Server{
		ID: 555, Name: "autoscaled-general-nbg1-abcde", ServerType: "cx33", Location: "nbg1",
		Labels: map[string]string{
			hcloudapi.LabelManagedBy: testCluster,
			hcloudapi.LabelNodeClaim: "autoscaled-general-nbg1-abcde",
		},
	}
	f.byName[existing.Name] = existing
	f.fail = func(int, hcloudapi.CreateServerRequest) error {
		return capacityErr(hcloud.ErrorCodeUniquenessError)
	}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	types := []*cloudprovider.InstanceType{instanceType("cx33", 4, 8, 8.49, "nbg1")}
	srv, _, err := p.Create(context.Background(), testNodeClass(), testNodeClaim(), types, "#cloud-config")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if srv.ID != 555 {
		t.Errorf("adopted server id %d, want the existing 555", srv.ID)
	}
}

// TestCreateRefusesToAdoptSomebodyElsesServer is the inverse and the dangerous
// direction: adopting on the name alone would hand karpenter a machine it does
// not own, which it would then manage and eventually delete.
func TestCreateRefusesToAdoptSomebodyElsesServer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"noLabels", nil},
		{"anotherCluster", map[string]string{hcloudapi.LabelManagedBy: "other"}},
		{"anotherNodeClaim", map[string]string{
			hcloudapi.LabelManagedBy: testCluster,
			hcloudapi.LabelNodeClaim: "a-different-claim",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServers()
			f.byName["autoscaled-general-nbg1-abcde"] = &hcloudapi.Server{
				ID: 999, Name: "autoscaled-general-nbg1-abcde", Labels: tc.labels,
			}
			f.fail = func(int, hcloudapi.CreateServerRequest) error {
				return capacityErr(hcloud.ErrorCodeUniquenessError)
			}
			p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

			types := []*cloudprovider.InstanceType{instanceType("cx33", 4, 8, 8.49, "nbg1")}
			if _, _, err := p.Create(context.Background(), testNodeClass(), testNodeClaim(), types, "#cloud-config"); err == nil {
				t.Fatal("adopted a server this cluster does not own")
			}
		})
	}
}

// TestCreateBoundsAttempts: a project genuinely out of capacity everywhere must
// not walk every candidate, because each attempt costs a POST and an action
// wait out of a rate limit shared with the CCM and the CSI driver.
func TestCreateBoundsAttempts(t *testing.T) {
	f := newFakeServers()
	f.fail = func(int, hcloudapi.CreateServerRequest) error {
		return capacityErr(hcloud.ErrorCodeResourceUnavailable)
	}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	var types []*cloudprovider.InstanceType
	for i := range 20 {
		types = append(types, instanceType(fmt.Sprintf("cx%02d", i), 4, 8, float64(i), "nbg1"))
	}

	if _, _, err := p.Create(context.Background(), testNodeClass(), testNodeClaim(), types, "#cloud-config"); err == nil {
		t.Fatal("expected exhaustion to be reported")
	}
	if got := len(f.attempts); got != DefaultMaxCreateAttempts {
		t.Errorf("made %d attempts, want the %d cap", got, DefaultMaxCreateAttempts)
	}
}

// TestCreateSkipsTypesThatCannotHoldTheClaim.
func TestCreateSkipsTypesThatCannotHoldTheClaim(t *testing.T) {
	f := newFakeServers()
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	claim := testNodeClaim()
	claim.Spec.Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: *resource.NewQuantity(8, resource.DecimalSI),
	}

	types := []*cloudprovider.InstanceType{
		instanceType("cx33", 4, 8, 8.49, "nbg1"), // too small, and cheapest
		instanceType("cx43", 8, 16, 15.99, "nbg1"),
	}

	srv, _, err := p.Create(context.Background(), testNodeClass(), claim, types, "#cloud-config")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if srv.ServerType != "cx43" {
		t.Errorf("took %q; the cheaper cx33 cannot hold an 8 cpu request", srv.ServerType)
	}
}

// TestOneTokenPerCreateNotPerAttempt.
//
// The userdata carries a live bootstrap token. Re-rendering per attempt would
// leave a valid cluster-join credential behind for every server type that
// happened to be out of stock.
func TestOneTokenPerCreateNotPerAttempt(t *testing.T) {
	f := newFakeServers()
	f.fail = func(n int, _ hcloudapi.CreateServerRequest) error {
		if n < 2 {
			return capacityErr(hcloud.ErrorCodeResourceUnavailable)
		}
		return nil
	}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	types := []*cloudprovider.InstanceType{
		instanceType("cx33", 4, 8, 8.49, "nbg1"),
		instanceType("cx43", 8, 16, 15.99, "nbg1"),
		instanceType("cx53", 16, 32, 29.49, "nbg1"),
	}
	if _, _, err := p.Create(context.Background(), testNodeClass(), testNodeClaim(), types, "#cloud-config token=abc"); err != nil {
		t.Fatal(err)
	}

	if len(f.attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(f.attempts))
	}
	for i, a := range f.attempts {
		if a.UserData != "#cloud-config token=abc" {
			t.Errorf("attempt %d carried different userdata; the token must be minted once per NodeClaim", i)
		}
	}
}

func TestDeleteRefusesUnmanagedServers(t *testing.T) {
	f := newFakeServers()
	// A control plane node, not created by this provider, so genuinely unlabelled.
	f.byID[1] = &hcloudapi.Server{ID: 1, Name: "k8s-node-1"}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	err := p.Delete(context.Background(), "hcloud://1")
	if err == nil {
		t.Fatal("deleted a server this cluster does not own")
	}
	var notManaged errNotManaged
	if !errors.As(err, &notManaged) {
		t.Errorf("err = %v, want an errNotManaged so callers can tell refusal from failure", err)
	}
	if len(f.deleted) != 0 {
		t.Error("the delete reached the API despite the ownership check")
	}
}

func TestDeleteRemovesManagedServers(t *testing.T) {
	f := newFakeServers()
	f.byID[2] = &hcloudapi.Server{ID: 2, Name: "worker", Labels: map[string]string{hcloudapi.LabelManagedBy: testCluster}}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	if err := p.Delete(context.Background(), "hcloud://2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != 2 {
		t.Errorf("deleted = %v, want [2]", f.deleted)
	}
}

// TestGetHidesUnmanagedServers: everything downstream of Get treats what it
// returns as a node karpenter may manage, so an unowned server must be
// invisible rather than merely undeletable.
func TestGetHidesUnmanagedServers(t *testing.T) {
	f := newFakeServers()
	f.byID[3] = &hcloudapi.Server{ID: 3, Name: "k8s-node-2"}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	srv, err := p.Get(context.Background(), "hcloud://3")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if srv != nil {
		t.Errorf("Get returned an unowned server %+v", srv)
	}
}

func TestListRefusesWithoutClusterName(t *testing.T) {
	f := newFakeServers()
	p := NewProvider(f, instancetype.NewUnavailable(), "")

	if _, err := p.List(context.Background()); err == nil {
		t.Fatal("listed with an empty selector, which matches the whole project")
	}
	if f.listCall != 0 {
		t.Error("the list reached the API with an empty selector")
	}
}

func TestProviderIDRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"hcloud://115379842", 115379842, false},
		{"hcloud://0", 0, true},
		// Trailing rubbish must be REJECTED, not truncated. A scan-based parse
		// accepts this and yields 115379842, which then selects a real server
		// that a delete would act on.
		{"hcloud://115379842abc", 0, true},
		{"hcloud://115379842 ", 0, true},
		{"hcloud://-5", 0, true},
		{"aws:///us-east-1a/i-abc", 0, true},
		{"hcloud://", 0, true},
		{"115379842", 0, true},
	} {
		got, err := hcloudapi.ServerIDFromProviderID(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ServerIDFromProviderID(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ServerIDFromProviderID(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

// coreNodeClaim builds a NodeClaim the way karpenter core actually does.
//
// Hand-building one is the trap this test exists to avoid. Core's
// NewNodeClaimTemplate stamps a nodeclass label into the template's labels and
// then folds those labels into spec.requirements, so every real NodeClaim
// carries `karpenter.itsh.dev/hcloudnodeclass In [<name>]`. A hand-written
// fixture has no requirements at all, which makes every requirement filter
// trivially satisfiable and hides exactly the class of bug this covers.
func coreNodeClaim(t *testing.T, types []*cloudprovider.InstanceType, extra ...corev1.NodeSelectorRequirement) *karpv1.NodeClaim {
	t.Helper()
	nodePool := &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaled-general-nbg1"},
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{
				Spec: karpv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpv1.NodeClassReference{
						Group: v1alpha1.Group, Kind: "HCloudNodeClass", Name: "default",
					},
					Requirements: []karpv1.NodeSelectorRequirementWithMinValues{},
				},
			},
		},
	}
	for _, r := range extra {
		nodePool.Spec.Template.Spec.Requirements = append(nodePool.Spec.Template.Spec.Requirements,
			karpv1.NodeSelectorRequirementWithMinValues{Key: r.Key, Operator: r.Operator, Values: r.Values})
	}
	nct := coresched.NewNodeClaimTemplate(nodePool)
	// ToNodeClaim serialises InstanceTypeOptions into an instance-type
	// requirement, which is how a real NodeClaim carries the candidate list the
	// scheduler settled on. Leaving it empty yields `instance-type In []`, which
	// matches nothing, so the fixture would be unlike anything production makes.
	nct.InstanceTypeOptions = types
	nc := nct.ToNodeClaim()
	nc.Name = "autoscaled-general-nbg1-abcde"
	return nc
}

// TestRealNodeClaimsProduceCandidates.
//
// The regression test for a filter that rejected EVERY instance type for EVERY
// real NodeClaim. Requirements.Compatible iterates the ARGUMENT's keys and
// rejects any non-well-known key the RECEIVER does not declare, so calling it
// as it.Requirements.Compatible(nodeClaimRequirements) fails on the nodeclass
// label core stamps into every NodeClaim. Create then reported insufficient
// capacity, core deleted the NodeClaim, and the cluster churned forever with
// every pod pending.
func TestRealNodeClaimsProduceCandidates(t *testing.T) {
	p := NewProvider(newFakeServers(), instancetype.NewUnavailable(), testCluster)
	types := []*cloudprovider.InstanceType{
		instanceType("cx33", 4, 8, 8.49, "nbg1", "fsn1"),
		instanceType("cx43", 8, 16, 15.99, "nbg1"),
	}

	claim := coreNodeClaim(t, types)
	got := p.orderedCandidates(claim, types)
	if len(got) == 0 {
		var keys []string
		for _, r := range claim.Spec.Requirements {
			keys = append(keys, r.Key)
		}
		t.Fatalf("a NodeClaim built the way core builds them produced NO candidates; "+
			"nothing would ever provision. Its requirement keys were %v", keys)
	}
	if got[0].InstanceType.Name != "cx33" {
		t.Errorf("cheapest candidate = %q, want cx33", got[0].InstanceType.Name)
	}
}

// TestRequirementsStillConstrain is the other half: the fix must not simply
// accept everything. Each of these must narrow the candidate set.
func TestRequirementsStillConstrain(t *testing.T) {
	p := NewProvider(newFakeServers(), instancetype.NewUnavailable(), testCluster)
	types := []*cloudprovider.InstanceType{
		instanceType("cx33", 4, 8, 8.49, "nbg1", "fsn1"),
		instanceType("cx43", 8, 16, 15.99, "nbg1"),
	}

	for _, tc := range []struct {
		name string
		req  corev1.NodeSelectorRequirement
		want []string
	}{
		{"regionPinned", corev1.NodeSelectorRequirement{
			Key: corev1.LabelTopologyRegion, Operator: corev1.NodeSelectorOpIn, Values: []string{"fsn1"},
		}, []string{"cx33@fsn1"}},
		{"typePinned", corev1.NodeSelectorRequirement{
			Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"cx43"},
		}, []string{"cx43@nbg1"}},
		{"impossibleType", corev1.NodeSelectorRequirement{
			Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"does-not-exist"},
		}, nil},
		{"impossibleRegion", corev1.NodeSelectorRequirement{
			Key: corev1.LabelTopologyRegion, Operator: corev1.NodeSelectorOpIn, Values: []string{"hil"},
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, c := range p.orderedCandidates(coreNodeClaim(t, types, tc.req), types) {
				got = append(got, c.InstanceType.Name+"@"+c.Location)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("candidates = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("candidates = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestUnavailableOfferingsAreSkipped: the unavailability cache is only useful
// if the candidate list actually honours it.
func TestUnavailableOfferingsAreSkipped(t *testing.T) {
	p := NewProvider(newFakeServers(), instancetype.NewUnavailable(), testCluster)
	types := []*cloudprovider.InstanceType{instanceType("cx33", 4, 8, 8.49, "nbg1")}
	types[0].Offerings[0].Available = false

	if got := p.orderedCandidates(coreNodeClaim(t, types), types); len(got) != 0 {
		t.Errorf("candidates = %+v, want none: the only offering is unavailable", got)
	}
}

// TestCandidateOrderIsStableAcrossInputOrder.
//
// The previous version of this test fed ONE input order and asserted the output
// did not change, which sort.SliceStable satisfies with no tiebreaks at all. To
// test the tiebreaks, the same set has to arrive in different orders.
func TestCandidateOrderIsStableAcrossInputOrder(t *testing.T) {
	p := NewProvider(newFakeServers(), instancetype.NewUnavailable(), testCluster)
	a := []*cloudprovider.InstanceType{
		instanceType("cx43", 8, 16, 15.99, "fsn1", "nbg1"),
		instanceType("cpx31", 8, 16, 15.99, "nbg1", "fsn1"),
	}
	b := []*cloudprovider.InstanceType{
		instanceType("cpx31", 8, 16, 15.99, "fsn1", "nbg1"),
		instanceType("cx43", 8, 16, 15.99, "nbg1", "fsn1"),
	}

	render := func(cs []Candidate) []string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.InstanceType.Name+"@"+c.Location)
		}
		return out
	}
	first := render(p.orderedCandidates(coreNodeClaim(t, a), a))
	second := render(p.orderedCandidates(coreNodeClaim(t, b), b))

	if len(first) != len(second) || len(first) == 0 {
		t.Fatalf("candidate sets differ in size: %v vs %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("equally priced candidates ordered differently for different input orders:\n  %v\n  %v\n"+
				"a replacement would pick a different type each pass and the fleet would never settle", first, second)
		}
	}
}

// TestIsManagedByFailsClosed pins the clause the file calls the single most
// important line in this provider. Delete has no cluster-name guard of its own,
// so an empty name must never match a server that merely has labels.
func TestIsManagedByFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		srv         *hcloudapi.Server
		clusterName string
	}{
		{"nilServer", nil, testCluster},
		{"noLabels", &hcloudapi.Server{ID: 1}, testCluster},
		{"emptyLabels", &hcloudapi.Server{ID: 1, Labels: map[string]string{}}, testCluster},
		{"labelsButNotOurs", &hcloudapi.Server{ID: 1, Labels: map[string]string{"env": "prod"}}, testCluster},
		{"otherCluster", &hcloudapi.Server{ID: 1, Labels: map[string]string{hcloudapi.LabelManagedBy: "other"}}, testCluster},
		// The important one: an empty cluster name must not match an empty or
		// absent label value.
		{"emptyClusterName", &hcloudapi.Server{ID: 1, Labels: map[string]string{"env": "prod"}}, ""},
		{"emptyClusterNameEmptyLabel", &hcloudapi.Server{ID: 1, Labels: map[string]string{hcloudapi.LabelManagedBy: ""}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.srv.IsManagedBy(tc.clusterName) {
				t.Errorf("IsManagedBy(%q) = true for %+v; this gates every delete and must fail closed", tc.clusterName, tc.srv)
			}
		})
	}

	owned := &hcloudapi.Server{ID: 1, Labels: map[string]string{hcloudapi.LabelManagedBy: testCluster}}
	if !owned.IsManagedBy(testCluster) {
		t.Error("IsManagedBy = false for a server this cluster genuinely owns")
	}
}

// TestCreateInvalidatesTheListCacheOnSuccess.
//
// Core's garbage collector compares its NodeClaims against List. A cache that
// predates a create shows no instance behind a live NodeClaim.
func TestCreateInvalidatesTheListCacheOnSuccess(t *testing.T) {
	f := newFakeServers()
	f.listed = []*hcloudapi.Server{}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	// Warm the cache.
	if _, err := p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.listCall

	types := []*cloudprovider.InstanceType{instanceType("cx33", 4, 8, 8.49, "nbg1")}
	srv, _, err := p.Create(context.Background(), testNodeClass(), coreNodeClaim(t, types), types, "#cloud-config")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.listed = []*hcloudapi.Server{srv}

	got, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.listCall == before {
		t.Fatal("List was served from a cache that predates the create; core's GC would see no instance behind a live NodeClaim")
	}
	if len(got) != 1 {
		t.Errorf("List = %+v after creating one server, want it present", got)
	}
}

// TestAdoptedServerReportsItsOwnType.
//
// The adopted server's type can differ from the candidate being attempted when
// the ordering changed between the lost create and the retry. Core never
// re-reads capacity from the Node, so publishing the attempted type binpacks
// every future pod against a machine that does not exist.
func TestAdoptedServerReportsItsOwnType(t *testing.T) {
	f := newFakeServers()
	f.byName["autoscaled-general-nbg1-abcde"] = &hcloudapi.Server{
		// Deliberately NOT the type the first attempt will try.
		//
		// Candidates are ordered cheapest-first, so attempt one is cx33. The
		// server that actually exists is a cx43, which is what a create whose
		// response was lost under a DIFFERENT ordering would have left behind.
		// If the fixture made these the same type, returning the attempted
		// candidate instead of the adopted one would produce an identical
		// answer and the test would prove nothing.
		ID: 555, Name: "autoscaled-general-nbg1-abcde", ServerType: "cx43", Location: "nbg1",
		Labels: map[string]string{
			hcloudapi.LabelManagedBy: testCluster,
			hcloudapi.LabelNodeClaim: "autoscaled-general-nbg1-abcde",
		},
	}
	// Every attempt collides with the existing name.
	f.fail = func(int, hcloudapi.CreateServerRequest) error {
		return capacityErr(hcloud.ErrorCodeUniquenessError)
	}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	types := []*cloudprovider.InstanceType{
		instanceType("cx43", 8, 16, 15.99, "nbg1"),
		instanceType("cx33", 4, 8, 8.49, "nbg1"),
	}
	srv, it, err := p.Create(context.Background(), testNodeClass(), coreNodeClaim(t, types), types, "#cloud-config")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if srv.ServerType != "cx43" {
		t.Fatalf("adopted %q, want the existing cx43", srv.ServerType)
	}
	if it == nil || it.Name != "cx43" {
		name := "<nil>"
		if it != nil {
			name = it.Name
		}
		t.Errorf("returned instance type %q for an adopted cx43; core never re-reads capacity from the Node, "+
			"so every future pod would be binpacked against the wrong machine", name)
	}
}

// ownedServer is a server carrying this cluster's ownership label.
func ownedServer(id int64, name string) *hcloudapi.Server {
	return &hcloudapi.Server{
		ID: id, Name: name, ServerType: "cx43", Location: "nbg1", Status: "running",
		Labels: map[string]string{hcloudapi.LabelManagedBy: testCluster},
	}
}

// TestGetIsServedFromTheListCache: drift calls Get once per NodeClaim per
// reconcile, which is a real share of Hetzner's 3600 requests/hour per project.
func TestGetIsServedFromTheListCache(t *testing.T) {
	f := newFakeServers()
	srv := ownedServer(7, "autoscaled-general-nbg1-aaaaa")
	f.listed = []*hcloudapi.Server{srv}
	f.byID[7] = srv
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	if _, err := p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.getCalls()

	got, err := p.Get(context.Background(), "hcloud://7")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.ID != 7 {
		t.Fatalf("Get = %+v, want the server with id 7", got)
	}
	if f.getCalls() != before {
		t.Errorf("Get went to the API for a server the warm listing already held (%d calls)", f.getCalls()-before)
	}
}

// TestGetFallsThroughToTheApiOnCacheMiss: a miss must never read as absence.
// Get's nil drops the Node's finalizer and skips the drain, so concluding it
// from a listing that merely predates the server strands that node's pods.
func TestGetFallsThroughToTheApiOnCacheMiss(t *testing.T) {
	f := newFakeServers()
	f.listed = []*hcloudapi.Server{ownedServer(7, "already-known")}
	// Present upstream, absent from the warm listing: exactly the just-created
	// server whose listing has not caught up.
	f.byID[9] = ownedServer(9, "created-after-the-listing")
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	if _, err := p.List(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := p.Get(context.Background(), "hcloud://9")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get reported a live server as gone because it was missing from a stale listing; termination would drop its finalizer undrained")
	}
	if f.getCalls() == 0 {
		t.Error("Get answered from the cache without a point read, so absence was inferred from the listing")
	}
}

// TestGetFallsThroughWhenTheCacheIsStale.
//
// Past the TTL the listing is not an answer, even for an id it contains.
func TestGetFallsThroughWhenTheCacheIsStale(t *testing.T) {
	f := newFakeServers()
	srv := ownedServer(7, "autoscaled-general-nbg1-aaaaa")
	f.listed = []*hcloudapi.Server{srv}
	f.byID[7] = srv
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	now := time.Now()
	p.cache = newListCache(f, DefaultListTTL, func() time.Time { return now })
	if _, err := p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.getCalls()

	now = now.Add(DefaultListTTL + time.Second)
	if _, err := p.Get(context.Background(), "hcloud://7"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if f.getCalls() == before {
		t.Error("Get served an expired listing")
	}
}

// TestGetHidesUnmanagedServersFromTheCache: the cache stores the raw listing,
// before List's ownership filter, so the cached path must re-check too.
func TestGetHidesUnmanagedServersFromTheCache(t *testing.T) {
	f := newFakeServers()
	unowned := &hcloudapi.Server{ID: 3, Name: "k8s-node-2"}
	f.listed = []*hcloudapi.Server{unowned}
	f.byID[3] = unowned
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	if _, err := p.List(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := p.Get(context.Background(), "hcloud://3")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("Get returned an unowned server %+v from the cache", got)
	}
}

// TestDeleteDoesNotUseTheListCache: the ownership gate in front of a
// destructive call reads live state, since labels can change out of band.
func TestDeleteDoesNotUseTheListCache(t *testing.T) {
	f := newFakeServers()
	srv := ownedServer(7, "autoscaled-general-nbg1-aaaaa")
	f.listed = []*hcloudapi.Server{srv}
	f.byID[7] = srv
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	if _, err := p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.getCalls()

	if err := p.Delete(context.Background(), "hcloud://7"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.getCalls() == before {
		t.Error("Delete gated a destructive call on a cached listing rather than live state")
	}
}

// TestGetReportsGenuineAbsence: the other half of the invariant. A warm cache
// must not stop Get reporting a server that really is gone.
func TestGetReportsGenuineAbsence(t *testing.T) {
	f := newFakeServers()
	f.listed = []*hcloudapi.Server{ownedServer(7, "still-here")}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	if _, err := p.List(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := p.Get(context.Background(), "hcloud://9")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("Get = %+v for a server that exists nowhere, want nil", got)
	}
	if f.getCalls() == 0 {
		t.Error("Get concluded absence from the cache without a point read")
	}
}

// TestGetIsSafeUnderConcurrentListAndInvalidate: get walks the shared slice, so
// it must hold the read lock while list replaces it and invalidate nils it.
// Nothing else exercises get concurrently, so a dropped RLock would go unseen.
func TestGetIsSafeUnderConcurrentListAndInvalidate(t *testing.T) {
	f := newFakeServers()
	srv := ownedServer(7, "autoscaled-general-nbg1-aaaaa")
	f.listed = []*hcloudapi.Server{srv}
	f.byID[7] = srv
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

	ctx := context.Background()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for range 300 {
				_, _ = p.List(ctx)
			}
		}()
		go func() {
			defer wg.Done()
			for range 300 {
				_, _ = p.Get(ctx, "hcloud://7")
			}
		}()
		go func() {
			defer wg.Done()
			for range 300 {
				p.cache.invalidate()
			}
		}()
	}
	wg.Wait()
}
