package instancegc

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

const testCluster = "itsh-prod"

var now = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

type fakeProvider struct {
	servers []*hcloudapi.Server
	deleted []string
	err     error
}

func (f *fakeProvider) List(context.Context) ([]*hcloudapi.Server, error) {
	return f.servers, f.err
}

func (f *fakeProvider) Delete(_ context.Context, providerID string) error {
	f.deleted = append(f.deleted, providerID)
	return nil
}

// nextID keeps server ids unique. Deriving one from the name length collides
// for equal-length names, which would give two servers the same providerID and
// let a multi-server test pass without proving anything.
var nextID int64

func server(name string, ageMinutes int, claim string) *hcloudapi.Server {
	nextID++
	s := &hcloudapi.Server{
		ID:      nextID,
		Name:    name,
		Created: now.Add(-time.Duration(ageMinutes) * time.Minute),
		Labels:  map[string]string{hcloudapi.LabelManagedBy: testCluster},
	}
	if claim != "" {
		s.Labels[hcloudapi.LabelNodeClaim] = claim
	}
	return s
}

func nodeClaim(name string) client.Object {
	return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func newTest(t *testing.T, p *fakeProvider, claims ...client.Object) *Controller {
	t.Helper()
	kubeClient := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(claims...).Build()
	return NewController(clocktesting.NewFakeClock(now), kubeClient, p, testCluster)
}

// TestReapsOrphans is the leak this controller exists to close: a server that
// booted with a valid join token, registered as a Node, and bills forever with
// no NodeClaim referring to it.
func TestReapsOrphans(t *testing.T) {
	p := &fakeProvider{servers: []*hcloudapi.Server{
		server("worker-1", 60, "worker-1"),
		server("orphan", 30, "orphan"),
	}}
	// A live NodeClaim alongside it, which is what a real leak looks like: one
	// stray server among working ones, never a fleet of them.
	c := newTest(t, p, nodeClaim("worker-1"))

	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(p.deleted) != 1 {
		t.Fatalf("deleted %v, want only the orphan removed", p.deleted)
	}
}

// TestRefusesToSweepWhenThereAreNoNodeClaimsAtAll.
//
// Zero NodeClaims against live servers is not a fleet of orphans. It is what a
// lost list, a wiped CRD, or somebody's `kubectl delete nodeclaims --all` looks
// like from in here, and acting on it deletes every node the cluster runs,
// undrained, in a single pass. A genuine leak is a handful of servers against a
// populated list, which the next sweep still catches.
func TestRefusesToSweepWhenThereAreNoNodeClaimsAtAll(t *testing.T) {
	p := &fakeProvider{servers: []*hcloudapi.Server{
		server("worker-1", 60, "worker-1"),
		server("worker-2", 60, "worker-2"),
		server("worker-3", 60, "worker-3"),
	}}
	c := newTest(t, p) // no NodeClaims at all

	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(p.deleted) != 0 {
		t.Errorf("deleted %v; an empty NodeClaim list must never authorise deleting the whole fleet", p.deleted)
	}
}

// TestSkipsServersWithNoCreationTime: an unknown age must not read as
// infinitely old on a path that deletes machines.
func TestSkipsServersWithNoCreationTime(t *testing.T) {
	orphan := server("orphan", 30, "orphan")
	orphan.Created = time.Time{}
	p := &fakeProvider{servers: []*hcloudapi.Server{server("worker-1", 60, "worker-1"), orphan}}
	c := newTest(t, p, nodeClaim("worker-1"))

	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(p.deleted) != 0 {
		t.Errorf("deleted %v, whose creation time is unknown; unknown age must mean cannot judge", p.deleted)
	}
}

// TestKeepsServersWithLiveNodeClaims: the ordinary case, and the one where a
// mistake deletes a running node out from under its workloads.
func TestKeepsServersWithLiveNodeClaims(t *testing.T) {
	p := &fakeProvider{servers: []*hcloudapi.Server{server("worker-1", 60, "worker-1")}}
	c := newTest(t, p, nodeClaim("worker-1"))

	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(p.deleted) != 0 {
		t.Errorf("deleted %v; that server has a live NodeClaim", p.deleted)
	}
}

// TestDoesNotReapServersYoungerThanTheFloor.
//
// The entire safety margin of this controller. A server exists BEFORE its
// NodeClaim records a providerID, so a healthy in-flight create legitimately
// has no NodeClaim yet. Without the floor this deletes the node it was just
// asked to build, then does it again, forever.
func TestDoesNotReapServersYoungerThanTheFloor(t *testing.T) {
	for _, age := range []int{0, 1, 4} {
		p := &fakeProvider{servers: []*hcloudapi.Server{
			server("worker-1", 60, "worker-1"),
			server("in-flight", age, "in-flight"),
		}}
		c := newTest(t, p, nodeClaim("worker-1"))

		if _, err := c.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(p.deleted) != 0 {
			t.Errorf("deleted a %d minute old server %v; it may be mid-create with its NodeClaim about to appear", age, p.deleted)
		}
	}
}

// TestMatchesOnNameNotProviderID.
//
// A NodeClaim exists before it records a providerID. Keying the live set on
// providerID would make every launching NodeClaim look like an orphan for the
// length of that window.
func TestMatchesOnNameNotProviderID(t *testing.T) {
	p := &fakeProvider{servers: []*hcloudapi.Server{server("launching", 30, "launching")}}
	// A NodeClaim that exists but has not recorded its providerID yet.
	c := newTest(t, p, nodeClaim("launching"))

	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(p.deleted) != 0 {
		t.Errorf("deleted %v; its NodeClaim exists but has no providerID yet", p.deleted)
	}
}

// TestIgnoresServersWithoutANodeClaimLabel: something else made it, or it
// predates this provider. Either way it is not ours to reap.
func TestIgnoresServersWithoutANodeClaimLabel(t *testing.T) {
	p := &fakeProvider{servers: []*hcloudapi.Server{
		server("worker-1", 60, "worker-1"),
		server("unlabelled", 120, ""),
	}}
	c := newTest(t, p, nodeClaim("worker-1"))

	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(p.deleted) != 0 {
		t.Errorf("deleted %v, which carries no nodeclaim label", p.deleted)
	}
}

// TestRefusesWithoutAClusterName: unreachable in a wired binary, checked
// because being wrong here deletes servers this cluster does not own.
func TestRefusesWithoutAClusterName(t *testing.T) {
	p := &fakeProvider{servers: []*hcloudapi.Server{server("orphan", 30, "orphan")}}
	kubeClient := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	c := NewController(clocktesting.NewFakeClock(now), kubeClient, p, "")

	if _, err := c.Reconcile(context.Background()); err == nil {
		t.Fatal("garbage collected with no cluster name")
	}
	if len(p.deleted) != 0 {
		t.Errorf("deleted %v despite having no cluster name", p.deleted)
	}
}

// TestRequeuesOnTheInterval keeps the sweep running: this is a singleton with
// no watch behind it, so a missing requeue means it runs exactly once.
func TestRequeuesOnTheInterval(t *testing.T) {
	// Both exits, because they are separate return statements: an early one for
	// "nothing to look at" and the ordinary one after the sweep. Covering only
	// the empty case leaves the path that actually does work able to fall
	// through without a requeue, and the sweep would then run exactly once.
	for _, tc := range []struct {
		name   string
		p      *fakeProvider
		claims []client.Object
	}{
		{"noServers", &fakeProvider{}, nil},
		{"serversAllLive", &fakeProvider{servers: []*hcloudapi.Server{server("worker-1", 60, "worker-1")}}, []client.Object{nodeClaim("worker-1")}},
		{"serversWithAnOrphan", &fakeProvider{servers: []*hcloudapi.Server{
			server("worker-1", 60, "worker-1"), server("orphan", 60, "orphan"),
		}}, []client.Object{nodeClaim("worker-1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTest(t, tc.p, tc.claims...)
			res, err := c.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if res.RequeueAfter != interval {
				t.Errorf("RequeueAfter = %v, want %v; without it the sweep never runs again", res.RequeueAfter, interval)
			}
		})
	}
}
