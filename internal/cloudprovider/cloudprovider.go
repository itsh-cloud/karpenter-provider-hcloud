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
// Never a package-level singleton: karpenter's pkg/utils/nodepool does
// client.Get INTO the object this returns, so a shared value would be
// concurrently mutated by every NodePool readiness reconcile.
func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.HCloudNodeClass{}}
}

// RepairPolicies returns the node conditions karpenter should treat as
// unhealthy enough to replace the machine.
//
// INERT unless the NodeRepair feature gate is on, which it is not by default.
//
// The durations are deliberately long, because node repair FORCE-terminates and
// does not respect a PodDisruptionBudget, while a kubelet restart, a control
// plane rollout or a network blip all resolve well inside thirty minutes.
func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return []cloudprovider.RepairPolicy{
		{
			// Well past the five minutes at which the node controller has
			// already tainted and evicted.
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			// The kubelet is not talking to the API server at all, which is
			// the shape a dead machine takes.
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

	// Rendering mints a bootstrap token, a live cluster-join credential written
	// into a kube-system Secret, so the candidate check comes first: otherwise
	// an unsatisfiable NodeClaim mints one on every retry.
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
// Karpenter's garbage collector compares this against its NodeClaims, so a
// server missing from here is one it deletes as an orphan, and a server wrongly
// included is one it may adopt.
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
// carry the availability.
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
// Karpenter binds pods against these labels before the node itself exists, so a
// missing one is a pod that will not schedule onto a node that could hold it.
func (c *CloudProvider) toNodeClaim(srv *hcloudapi.Server, it *cloudprovider.InstanceType, in *karpv1.NodeClaim, nodeClass *v1alpha1.HCloudNodeClass) *karpv1.NodeClaim {
	out := in.DeepCopy()

	// The spec hash, stamped HERE and nowhere else on the launch path. Without
	// it NodeClassDrift is dead code: the hash controller only writes this
	// annotation during a version back-fill, so nothing launched afterwards
	// carries one and drift declines to judge forever. Core merges these onto
	// the stored object in PopulateNodeClaimDetails.
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
	// Agrees with the node by construction: hcloud-CCM derives this label from
	// the LOCATION with a pure function and never reads the real datacenter.
	// Carrying it here lets an in-flight NodeClaim be priced and satisfy a
	// zone-constrained pod before its Node exists.
	out.Labels[corev1.LabelTopologyZone] = instancetype.LegacyDatacenterForLocation(srv.Location)

	out.Status.ProviderID = srv.ProviderID()
	out.CreationTimestamp = metav1.Time{Time: srv.Created}
	return out
}

// toNodeClaimFromServer builds a NodeClaim for Get and List, where there may be
// no in-cluster NodeClaim to copy from.
//
// Capacity comes from the catalog rather than being left empty: core's garbage
// collector and its cost accounting both read it.
func (c *CloudProvider) toNodeClaimFromServer(srv *hcloudapi.Server) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              srv.Name,
			CreationTimestamp: metav1.Time{Time: srv.Created},
			Labels: map[string]string{
				corev1.LabelInstanceTypeStable: srv.ServerType,
				corev1.LabelTopologyRegion:     srv.Location,
				corev1.LabelTopologyZone:       instancetype.LegacyDatacenterForLocation(srv.Location),
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
// It applies the SAME VM overhead correction instancetype.Capacity does, or Get
// and List would report the same server as roughly 7% larger than Create did,
// and core's cost accounting and its garbage collector both read this. Kubelet
// configuration cannot be consulted (there is no NodeClass here), so this uses
// the defaults; the exact per-NodeClass figure lives on the Node.
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
