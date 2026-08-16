// Package cloudprovider implements karpenter's CloudProvider for Hetzner Cloud.
package cloudprovider

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
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
// unhealthy enough to replace the machine.
//
// Declared, but INERT unless the NodeRepair feature gate is enabled: core only
// registers the health controller when both this is non-empty and the gate is
// on, and the gate is off by default. Declaring them now means enabling repair
// later is one flag rather than a code change, and it puts the durations
// somewhere reviewable instead of in an operator's head.
//
// The durations are deliberately long. Node repair FORCE-terminates: it does
// not respect a PodDisruptionBudget, because the premise is that the node is
// already unusable. That makes a false positive expensive, and the common
// causes of a node briefly reporting NotReady, a kubelet restart, a control
// plane rollout, a network blip, all resolve well inside thirty minutes. Being
// slow to repair a genuinely dead node costs one node's capacity; being quick
// to repair a live one costs its workloads their disruption budget.
func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return []cloudprovider.RepairPolicy{
		{
			// The kubelet has stopped reporting. Thirty minutes is well past
			// the five-minute mark at which the node controller has already
			// tainted and evicted, so anything still here is not coming back.
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			// Unknown means the kubelet is not talking to the API server at
			// all, which is the shape a dead machine takes.
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionUnknown,
			TolerationDuration: 30 * time.Minute,
		},
	}
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
	return c.toNodeClaim(srv, instanceType, nodeClaim, nodeClass), nil
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

// nodeClassKey is where a NodeClaim's NodeClass lives. Cluster-scoped, so the
// namespace is always empty.
func nodeClassKey(nodeClaim *karpv1.NodeClaim) types.NamespacedName {
	if nodeClaim.Spec.NodeClassRef == nil {
		return types.NamespacedName{}
	}
	return types.NamespacedName{Name: nodeClaim.Spec.NodeClassRef.Name}
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
func (c *CloudProvider) toNodeClaim(srv *hcloudapi.Server, it *cloudprovider.InstanceType, in *karpv1.NodeClaim, nodeClass *v1alpha1.HCloudNodeClass) *karpv1.NodeClaim {
	out := in.DeepCopy()

	// The spec hash, stamped HERE and nowhere else on the launch path.
	//
	// Without it NodeClassDrift is dead code: the hash controller only writes
	// this annotation onto NodeClaims during a version back-fill, which runs
	// once and then never again, so no NodeClaim launched afterwards carries
	// one and drift declines to judge forever. Everything the hash exists to
	// cover, user data, ssh keys, kubelet config, bootstrap, would silently
	// never drift a node.
	//
	// Core picks these up in PopulateNodeClaimDetails, which merges the
	// annotations of the NodeClaim returned from Create onto the stored object.
	if nodeClass != nil {
		out.Annotations = lo.Assign(out.Annotations, map[string]string{
			v1alpha1.AnnotationHash:        nodeClass.Hash(),
			v1alpha1.AnnotationHashVersion: v1alpha1.HashVersion,
		})
	}
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
