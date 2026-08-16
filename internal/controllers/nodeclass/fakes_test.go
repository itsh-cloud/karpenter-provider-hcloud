package nodeclass

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/catalog"
)

// transientErr is an hcloud error this provider classifies as transient. Used
// wherever a test needs "the API failed" as distinct from "the API answered and
// the answer was no".
var transientErr = hcloud.Error{Code: hcloud.ErrorCodeRateLimitExceeded, Message: "rate limit exceeded"}

// fatalErr is a rejected credential, which is permanent rather than transient.
var fatalErr = hcloud.Error{Code: hcloud.ErrorCodeUnauthorized, Message: "unable to authenticate"}

// fakeResources is a hcloudapi.Resources whose every method can be overridden
// per test. The zero value resolves everything successfully.
type fakeResources struct {
	image          func() (*hcloudapi.Image, error)
	network        func() (*hcloudapi.Network, error)
	firewall       func(name string) (*hcloudapi.Firewall, error)
	sshKey         func(name string) (*hcloudapi.SSHKey, error)
	placementGroup func() (*hcloudapi.PlacementGroup, error)
}

func (f *fakeResources) Image(_ context.Context, name string, _ *int64, arch string) (*hcloudapi.Image, error) {
	if f.image != nil {
		return f.image()
	}
	return &hcloudapi.Image{ID: 42, Name: name, Architecture: arch, Created: "2026-01-01T00:00:00Z"}, nil
}

func (f *fakeResources) Network(_ context.Context, name string, _ *int64) (*hcloudapi.Network, error) {
	if f.network != nil {
		return f.network()
	}
	return &hcloudapi.Network{ID: 7, Name: name, IPRange: "10.0.0.0/16", Zone: "eu-central"}, nil
}

func (f *fakeResources) Firewall(_ context.Context, name string, _ *int64) (*hcloudapi.Firewall, error) {
	if f.firewall != nil {
		return f.firewall(name)
	}
	return &hcloudapi.Firewall{ID: 1, Name: name}, nil
}

func (f *fakeResources) SSHKey(_ context.Context, name string, _ *int64) (*hcloudapi.SSHKey, error) {
	if f.sshKey != nil {
		return f.sshKey(name)
	}
	return &hcloudapi.SSHKey{ID: 2, Name: name, Fingerprint: "aa:bb"}, nil
}

func (f *fakeResources) PlacementGroup(_ context.Context, name string, _ *int64) (*hcloudapi.PlacementGroup, error) {
	if f.placementGroup != nil {
		return f.placementGroup()
	}
	return &hcloudapi.PlacementGroup{ID: 3, Name: name, Type: "spread", ServerCount: 1}, nil
}

// fakeDiscovery is a nodeclass.Discovery with scriptable failures.
type fakeDiscovery struct {
	refreshErr error
	endpoint   string
	hashes     []string
	resolveErr error
}

func (f *fakeDiscovery) Refresh(context.Context) error { return f.refreshErr }

func (f *fakeDiscovery) Resolve(endpointOverride string, hashOverride []string) (string, []string, error) {
	if f.resolveErr != nil {
		return "", nil, f.resolveErr
	}
	endpoint, hashes := f.endpoint, f.hashes
	if endpointOverride != "" {
		endpoint = endpointOverride
	}
	if len(hashOverride) > 0 {
		hashes = hashOverride
	}
	return endpoint, hashes, nil
}

func workingDiscovery() *fakeDiscovery {
	return &fakeDiscovery{
		endpoint: "10.0.0.2:6443",
		hashes:   []string{"sha256:" + strings.Repeat("a", 64)},
	}
}

// fakeCatalog serves a fixed snapshot.
type fakeCatalog struct{ snapshot *catalog.Snapshot }

func (f *fakeCatalog) Get() *catalog.Snapshot { return f.snapshot }

// twoLocationCatalog offers one x86 server type in nbg1 and fsn1, both in
// eu-central, plus a type in a second network zone so that zone filtering has
// something to filter.
func twoLocationCatalog() *fakeCatalog {
	return &fakeCatalog{snapshot: &catalog.Snapshot{
		Generation: 1,
		ServerTypes: []hcloudapi.ServerType{{
			Name:         "cx23",
			Architecture: "x86",
			Locations: []hcloudapi.ServerTypeLocation{
				{Location: "nbg1", NetworkZone: "eu-central"},
				{Location: "fsn1", NetworkZone: "eu-central"},
				{Location: "hil", NetworkZone: "us-west"},
			},
		}},
	}}
}

// fakeRecorder collects published events.
type fakeRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *fakeRecorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, evts...)
}

func (r *fakeRecorder) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Reason)
	}
	return out
}

// countingClient counts writes so a test can assert that an idle reconcile
// performs none. Write counting is the only way to catch status churn: the
// object still looks correct after a redundant write, so every other assertion
// passes while the controller hot-loops against its own watch.
// Counters are atomic so that a concurrent-reconcile test can be added without
// tripping -race on the harness itself.
type countingClient struct {
	client.Client
	patches       atomic.Int64
	statusPatches atomic.Int64
}

func (c *countingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches.Add(1)
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *countingClient) Status() client.SubResourceWriter {
	return &countingStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type countingStatusWriter struct {
	client.SubResourceWriter
	parent *countingClient
}

func (w *countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.parent.statusPatches.Add(1)
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// emptyCatalog is a successful fetch that yielded nothing usable. Reachable
// with no error at all: ServerTypes drops any location entry whose Location
// pointer is nil, which is the field shape Hetzner is changing.
func emptyCatalog() *fakeCatalog {
	return &fakeCatalog{snapshot: &catalog.Snapshot{Generation: 1}}
}
