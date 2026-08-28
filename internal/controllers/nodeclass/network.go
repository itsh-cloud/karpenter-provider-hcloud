package nodeclass

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// Network resolves spec.networkSelector into status.network.
type Network struct {
	clk       clock.Clock
	resources hcloudapi.Resources
}

// NewNetwork returns the network sub-reconciler.
func NewNetwork(clk clock.Clock, resources hcloudapi.Resources) *Network {
	return &Network{clk: clk, resources: resources}
}

func (n *Network) Reconcile(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	sel := nodeClass.Spec.NetworkSelector
	if sel == nil {
		// An optional selector left unset resolves to True, never Unknown: an
		// Unknown dependent pins the Ready roll-up at Unknown forever, which
		// blocks provisioning as effectively as a hard failure.
		nodeClass.Status.Network = nil
		nodeClass.StatusConditions(status.WithClock(n.clk)).SetTrue(v1alpha1.ConditionTypeNetworkReady)
		return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
	}

	net, err := n.resources.Network(ctx, sel.Name, sel.ID)
	if err != nil {
		if reason, ok := configFailure(ctx, err); ok {
			nodeClass.Status.Network = nil
			nodeClass.StatusConditions(status.WithClock(n.clk)).SetFalse(
				v1alpha1.ConditionTypeNetworkReady, "Network"+reason,
				failureMessage("networkSelector "+describeSelector(sel.Name, sel.ID), reason, err),
			)
			return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
		}
		noteUnreachable(nodeClass.StatusConditions(status.WithClock(n.clk)), v1alpha1.ConditionTypeNetworkReady, "Network", err)
		return reconcile.Result{}, fmt.Errorf("resolving network, %w", err)
	}

	if net.Zone == "" {
		// A network with no subnet has no zone and no server can attach to it.
		// Reported here rather than left to surface as a generic invalid_input
		// on every NodeClaim at create time.
		nodeClass.Status.Network = nil
		nodeClass.StatusConditions(status.WithClock(n.clk)).SetFalse(
			v1alpha1.ConditionTypeNetworkReady, "NetworkHasNoSubnet",
			fmt.Sprintf("network %s has no subnet, so it has no network zone and no server can attach to it", describeSelector(sel.Name, sel.ID)),
		)
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	nodeClass.Status.Network = &v1alpha1.NetworkStatus{
		ID:      net.ID,
		Name:    net.Name,
		IPRange: net.IPRange,
		Zone:    net.Zone,
	}
	nodeClass.StatusConditions(status.WithClock(n.clk)).SetTrue(v1alpha1.ConditionTypeNetworkReady)
	return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
}
