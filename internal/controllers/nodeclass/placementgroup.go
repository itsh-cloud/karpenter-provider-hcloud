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

const (
	// PlacementGroupMaxServers is Hetzner's hard cap on members of a spread
	// placement group. The cap is not advertised anywhere in the API response;
	// exceeding it returns placement_error on create.
	PlacementGroupMaxServers = 10

	// PlacementGroupWarnServers is where the condition starts warning.
	//
	// Two members of headroom, because that is roughly what a rolling
	// replacement needs: karpenter launches the replacement before terminating
	// the node it replaces, so a group sitting at the cap cannot be rolled at
	// all, and the failure it produces is placement_error, which this provider
	// classifies as a capacity error and which is genuinely indistinguishable
	// from a stockout. Nothing downstream can tell the operator what is wrong,
	// so it has to be said here, before it happens.
	PlacementGroupWarnServers = PlacementGroupMaxServers - 2
)

// PlacementGroup resolves spec.placementGroup into status.placementGroup.
type PlacementGroup struct {
	clk       clock.Clock
	resources hcloudapi.Resources
}

// NewPlacementGroup returns the placement group sub-reconciler.
func NewPlacementGroup(clk clock.Clock, resources hcloudapi.Resources) *PlacementGroup {
	return &PlacementGroup{clk: clk, resources: resources}
}

func (p *PlacementGroup) Reconcile(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	sel := nodeClass.Spec.PlacementGroup
	if sel == nil {
		nodeClass.Status.PlacementGroup = nil
		nodeClass.StatusConditions(status.WithClock(p.clk)).SetTrue(v1alpha1.ConditionTypePlacementGroupReady)
		return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
	}

	pg, err := p.resources.PlacementGroup(ctx, sel.Name, sel.ID)
	if err != nil {
		if reason, ok := configFailure(ctx, err); ok {
			nodeClass.Status.PlacementGroup = nil
			nodeClass.StatusConditions(status.WithClock(p.clk)).SetFalse(
				v1alpha1.ConditionTypePlacementGroupReady, "PlacementGroup"+reason,
				failureMessage("placementGroup "+describeSelector(sel.Name, sel.ID), reason, err),
			)
			return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
		}
		noteUnreachable(nodeClass.StatusConditions(status.WithClock(p.clk)), v1alpha1.ConditionTypePlacementGroupReady, "PlacementGroup", err)
		return reconcile.Result{}, fmt.Errorf("resolving placement group, %w", err)
	}

	nodeClass.Status.PlacementGroup = &v1alpha1.PlacementGroupStatus{
		ID:          pg.ID,
		Name:        pg.Name,
		Type:        pg.Type,
		ServerCount: pg.ServerCount,
	}

	// The condition stays True at and beyond the cap rather than going False.
	// False would take Ready to False, which makes karpenter core DELETE
	// in-flight NodeClaims and stops the NodePool entirely, and the only way a
	// full group empties is by those same nodes being removed. The warning is
	// carried in the reason and message instead, which is what an operator
	// alerts on and what `kubectl describe` shows.
	//
	// The exact count is in status.placementGroup.serverCount and deliberately
	// not in the message: the message is bucketed so that the CONDITION does
	// not rewrite itself on every node created or removed.
	conds := nodeClass.StatusConditions(status.WithClock(p.clk))
	switch {
	case pg.ServerCount >= PlacementGroupMaxServers:
		conds.SetTrueWithReason(v1alpha1.ConditionTypePlacementGroupReady, "PlacementGroupAtCapacity",
			fmt.Sprintf("placement group is at Hetzner's limit of %d servers; the next create fails with placement_error, which is indistinguishable from a stockout", PlacementGroupMaxServers))
	case pg.ServerCount >= PlacementGroupWarnServers:
		conds.SetTrueWithReason(v1alpha1.ConditionTypePlacementGroupReady, "PlacementGroupNearCapacity",
			fmt.Sprintf("placement group is within %d servers of Hetzner's limit of %d; at the limit creates fail with placement_error, which is indistinguishable from a stockout", PlacementGroupMaxServers-PlacementGroupWarnServers, PlacementGroupMaxServers))
	default:
		conds.SetTrue(v1alpha1.ConditionTypePlacementGroupReady)
	}
	return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
}
