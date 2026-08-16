// Package cloudprovider implements karpenter's CloudProvider for Hetzner Cloud.
package cloudprovider

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/catalog"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instance"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instancetype"
)

// Name is how this provider identifies itself in karpenter's metrics and logs.
const Name = "hcloud"

// CloudProvider implements sigs.k8s.io/karpenter's CloudProvider.
var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

// CloudProvider turns karpenter's scheduling decisions into Hetzner servers.
type CloudProvider struct {
	kubeClient   client.Client
	instances    *instance.Provider
	catalog      CatalogProvider
	unavailable  *instancetype.Unavailable
	bootstrapper Bootstrapper
	clusterName  string
}

// CatalogProvider is the slice of the catalog provider this needs.
type CatalogProvider interface {
	Get() *catalog.Snapshot
}

// Bootstrapper renders the cloud-init a new server boots with.
type Bootstrapper interface {
	Render(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass, nodeClaim *karpv1.NodeClaim) (string, error)
}

// New returns the Hetzner CloudProvider.
func New(
	kubeClient client.Client,
	instances *instance.Provider,
	catalogProvider CatalogProvider,
	unavailable *instancetype.Unavailable,
	bootstrapper Bootstrapper,
	clusterName string,
) *CloudProvider {
	return &CloudProvider{
		kubeClient:   kubeClient,
		instances:    instances,
		catalog:      catalogProvider,
		unavailable:  unavailable,
		bootstrapper: bootstrapper,
		clusterName:  clusterName,
	}
}

func (c *CloudProvider) Name() string { return Name }

// GetSupportedNodeClasses returns a FRESH object each call.
//
// Not a package-level singleton, which is the trap here: karpenter's
// pkg/utils/nodepool does client.Get INTO the object this returns, and the
// readiness controller passes the same object to builder.Watches. A shared
// value would be concurrently mutated by every NodePool readiness reconcile.
func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.HCloudNodeClass{}}
}

// RepairPolicies returns the node conditions karpenter should treat as
// unhealthy.
//
// Deliberately empty for now. Node repair force-terminates nodes on a timer,
// and enabling it before the provisioning path has run in production would let
// a misdiagnosis delete healthy machines. Phase 7 turns it on with explicit
// tolerations.
func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return nil
}

// Create launches a server for the NodeClaim and returns it hydrated with the
// labels karpenter needs to bind pods to it.
func (c *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	nodeClass, err := c.resolveNodeClass(ctx, nodeClaim)
	if err != nil {
		return nil, err
	}

	instanceTypes, err := c.instanceTypes(nodeClass)
	if err != nil {
		return nil, err
	}

	// Rendering mints a bootstrap token, which is a live cluster-join
	// credential written into a kube-system Secret. Doing it before there is
	// anything worth attempting would mint one for a NodeClaim that cannot be
	// satisfied at all, on every retry, so the check comes first.
	if !c.instances.HasCandidates(nodeClaim, instanceTypes) {
		return nil, cloudprovider.NewInsufficientCapacityError(
			fmt.Errorf("no instance type in this NodeClaim's requirements is offered in any location nodeclass %q allows", nodeClass.Name))
	}

	userData, err := c.bootstrapper.Render(ctx, nodeClass, nodeClaim)
	if err != nil {
		return nil, cloudprovider.NewCreateError(err, "BootstrapRenderFailed", err.Error())
	}

	srv, instanceType, err := c.instances.Create(ctx, nodeClass, nodeClaim, instanceTypes, userData)
	if err != nil {
		return nil, err
	}
	return c.toNodeClaim(srv, instanceType, nodeClaim), nil
}

// Delete removes the server behind a NodeClaim.
//
// Karpenter retries until this reports not-found, so "already gone" must be
// reported as NodeClaimNotFoundError rather than as success or as an error.
func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	if nodeClaim.Status.ProviderID == "" {
		// Nothing was ever launched, so there is nothing to remove. Reported as
		// not-found so core stops retrying and finishes terminating.
		return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("nodeclaim %q has no providerID", nodeClaim.Name))
	}
	err := c.instances.Delete(ctx, nodeClaim.Status.ProviderID)
	switch {
	case err == nil:
		return nil
	case hcloudapi.IsNotFound(err):
		return cloudprovider.NewNodeClaimNotFoundError(err)
	default:
		return err
	}
}

// Get returns the NodeClaim backing a provider id.
func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	srv, err := c.instances.Get(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("server %q not found", providerID))
	}
	return c.toNodeClaimFromServer(srv), nil
}

// List returns a NodeClaim for every server this cluster's provider owns.
//
// This is what karpenter's garbage collector compares against its NodeClaims,
// so a server missing from here is one it will consider an orphan and delete,
// and a server wrongly included is one it may adopt.
func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	servers, err := c.instances.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*karpv1.NodeClaim, 0, len(servers))
	for _, srv := range servers {
		out = append(out, c.toNodeClaimFromServer(srv))
	}
	return out, nil
}

// GetInstanceTypes returns every instance type, including unavailable ones.
//
// The contract says every type regardless of availability: karpenter needs to
// know a type exists in order to explain why a pod is unschedulable. Offerings
// carry the availability, which is where the unavailability cache lands.
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	nodeClass, err := c.nodeClassForNodePool(ctx, nodePool)
	if err != nil {
		return nil, err
	}
	return c.instanceTypes(nodeClass)
}

func (c *CloudProvider) instanceTypes(nodeClass *v1alpha1.HCloudNodeClass) ([]*cloudprovider.InstanceType, error) {
	snapshot := c.catalog.Get()
	if snapshot == nil {
		// Never an empty list. Zero instance types tells karpenter the cluster
		// can hold nothing, which presents as every pending pod being
		// permanently unschedulable rather than as a transient outage.
		return nil, fmt.Errorf("the Hetzner server type catalog has not been fetched yet")
	}
	return instancetype.Build(snapshot.ServerTypes, nodeClass, snapshot.PrimaryIPv4Monthly, c.unavailable, instancetype.Options{}), nil
}

// resolveNodeClass loads the NodeClass a NodeClaim points at and refuses to
// launch against one that is not ready.
func (c *CloudProvider) resolveNodeClass(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*v1alpha1.HCloudNodeClass, error) {
	ref := nodeClaim.Spec.NodeClassRef
	if ref == nil {
		return nil, cloudprovider.NewNodeClassNotReadyError(fmt.Errorf("nodeclaim %q has no nodeClassRef", nodeClaim.Name))
	}
	nodeClass := &v1alpha1.HCloudNodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: ref.Name}, nodeClass); err != nil {
		return nil, cloudprovider.NewNodeClassNotReadyError(fmt.Errorf("getting nodeclass %q, %w", ref.Name, err))
	}
	if !nodeClass.DeletionTimestamp.IsZero() {
		return nil, cloudprovider.NewNodeClassNotReadyError(fmt.Errorf("nodeclass %q is terminating", ref.Name))
	}
	// The readiness gate. Every id the create uses comes from status, and
	// status is only trustworthy once the roll-up says so.
	if !nodeClass.StatusConditions().Root().IsTrue() {
		return nil, cloudprovider.NewNodeClassNotReadyError(
			fmt.Errorf("nodeclass %q is not ready: %s", ref.Name, nodeClass.StatusConditions().Root().Message))
	}
	return nodeClass, nil
}

func (c *CloudProvider) nodeClassForNodePool(ctx context.Context, nodePool *karpv1.NodePool) (*v1alpha1.HCloudNodeClass, error) {
	if nodePool == nil || nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return nil, fmt.Errorf("nodepool has no nodeClassRef")
	}
	nodeClass := &v1alpha1.HCloudNodeClass{}
	name := nodePool.Spec.Template.Spec.NodeClassRef.Name
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: name}, nodeClass); err != nil {
		return nil, fmt.Errorf("getting nodeclass %q, %w", name, err)
	}
	return nodeClass, nil
}

// toNodeClaim hydrates the NodeClaim karpenter gets back from Create.
//
// The labels here are what karpenter binds pods against before the node itself
// exists, so a missing one is a pod that will not schedule onto a node that
// could have held it.
func (c *CloudProvider) toNodeClaim(srv *hcloudapi.Server, it *cloudprovider.InstanceType, in *karpv1.NodeClaim) *karpv1.NodeClaim {
	out := in.DeepCopy()
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}

	// Every requirement the instance type pins to a single value becomes a
	// label, which is how core's binpacking learns what it just got.
	if it != nil {
		for key, req := range it.Requirements {
			if req.Len() == 1 {
				out.Labels[key] = req.Values()[0]
			}
		}
		out.Status.Capacity = it.Capacity
		out.Status.Allocatable = it.Allocatable()
	}

	// Written after the requirements, so what the server actually is wins over
	// what the instance type asked for.
	out.Labels[corev1.LabelInstanceTypeStable] = srv.ServerType
	out.Labels[corev1.LabelTopologyRegion] = srv.Location
	out.Labels[v1alpha1.LabelCSILocation] = srv.Location
	out.Labels[karpv1.CapacityTypeLabelKey] = karpv1.CapacityTypeOnDemand
	// topology.kubernetes.io/zone is deliberately NOT set. hcloud-CCM writes it
	// from the datacenter the server actually landed in, and guessing here
	// would produce a label that disagrees with the node and marks it
	// permanently drifted.

	out.Status.ProviderID = srv.ProviderID()
	out.CreationTimestamp = metav1.Time{Time: srv.Created}
	return out
}

// toNodeClaimFromServer builds a NodeClaim for Get and List, where there may be
// no in-cluster NodeClaim to copy from.
//
// Capacity is looked up from the catalog rather than left empty: core's
// garbage collector and its cost accounting both read it.
func (c *CloudProvider) toNodeClaimFromServer(srv *hcloudapi.Server) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              srv.Name,
			CreationTimestamp: metav1.Time{Time: srv.Created},
			Labels: map[string]string{
				corev1.LabelInstanceTypeStable: srv.ServerType,
				corev1.LabelTopologyRegion:     srv.Location,
				v1alpha1.LabelCSILocation:      srv.Location,
				karpv1.CapacityTypeLabelKey:    karpv1.CapacityTypeOnDemand,
			},
		},
		Status: karpv1.NodeClaimStatus{ProviderID: srv.ProviderID()},
	}
	if pool := srv.Labels[hcloudapi.LabelNodePool]; pool != "" {
		nc.Labels[karpv1.NodePoolLabelKey] = pool
	}
	if capacity := c.capacityFor(srv.ServerType); capacity != nil {
		nc.Status.Capacity = capacity
	}
	return nc
}

// capacityFor returns a server type's capacity from the catalog, or nil when
// the catalog cannot answer.
//
// It applies the SAME VM overhead correction instancetype.Capacity does, and
// that consistency is the point rather than a detail. Create publishes
// Status.Capacity from the instance type, which is corrected; if this returned
// Hetzner's advertised figures instead, the same server would report roughly
// 7% more memory through Get and List than it did through Create, and core's
// cost accounting and its garbage collector both read this.
//
// Kubelet configuration is deliberately not consulted. Get and List are given a
// server, not a NodeClass, so there is no way to know which kubelet block built
// it; this uses the defaults, which is exact for every NodeClass that does not
// override them and close for the rest. The precise per-NodeClass figure lives
// on the Node itself, which is where anything needing exactness should read it.
func (c *CloudProvider) capacityFor(serverType string) corev1.ResourceList {
	snapshot := c.catalog.Get()
	if snapshot == nil {
		return nil
	}
	for _, st := range snapshot.ServerTypes {
		if st.Name != serverType {
			continue
		}
		return instancetype.Capacity(
			st.Cores, st.MemoryGiB, st.DiskGB,
			v1alpha1.DefaultMaxPods,
			instancetype.DefaultVMMemoryOverheadPercent,
			instancetype.DefaultVMDiskOverheadPercent,
		)
	}
	return nil
}

// IsDrifted reports whether a NodeClaim no longer matches what would be
// provisioned for it today.
//
// Deliberately inert in this phase, and that is a safety decision rather than
// an omission. Drift is the CloudProvider method most able to do damage by
// being wrong: a false positive replaces the fleet. The machinery it needs is
// already in place, the hash controller keeps
// karpenter.itsh.dev/hcloudnodeclass-hash on both the class and its NodeClaims,
// but turning it on belongs with the drift tests and the metrics that make a
// mistaken replacement visible while it is happening.
//
// # This does NOT mean nothing deletes nodes
//
// Read alone, "drift is off" invites the conclusion that this deployment
// cannot replace a node. It can. Registering karpenter core's controllers
// turns on, unconditionally:
//
//   - consolidation, whose NodePool defaults are WhenEmptyOrUnderutilized with
//     consolidateAfter 0s, so an empty or underutilised node is deleted and
//     replaced as soon as it qualifies;
//   - expiration, default expireAfter 720h;
//   - termination, which drains and evicts.
//
// Only drift and node repair are off. A NodePool that should not disrupt
// anything yet has to say so itself, with spec.disruption.budgets [{nodes:
// "0"}], which is how the first one here ships.
func (c *CloudProvider) IsDrifted(_ context.Context, _ *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", nil
}
