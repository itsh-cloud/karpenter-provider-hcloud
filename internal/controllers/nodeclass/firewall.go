package nodeclass

import (
	"context"
	"fmt"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// Firewalls resolves spec.firewallSelectors into status.firewalls.
type Firewalls struct {
	clk       clock.Clock
	resources hcloudapi.Resources
}

// NewFirewalls returns the firewall sub-reconciler.
func NewFirewalls(clk clock.Clock, resources hcloudapi.Resources) *Firewalls {
	return &Firewalls{clk: clk, resources: resources}
}

func (f *Firewalls) Reconcile(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	if len(nodeClass.Spec.FirewallSelectors) == 0 {
		nodeClass.Status.Firewalls = nil
		nodeClass.StatusConditions(status.WithClock(f.clk)).SetTrue(v1alpha1.ConditionTypeFirewallsReady)
		return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
	}

	resolved := make([]v1alpha1.FirewallStatus, 0, len(nodeClass.Spec.FirewallSelectors))
	var missing []string
	var reason string
	var cause error

	for _, sel := range nodeClass.Spec.FirewallSelectors {
		fw, err := f.resources.Firewall(ctx, sel.Name, sel.ID)
		if err != nil {
			r, ok := configFailure(ctx, err)
			if !ok {
				noteUnreachable(nodeClass.StatusConditions(status.WithClock(f.clk)), v1alpha1.ConditionTypeFirewallsReady, "Firewalls", err)
				return reconcile.Result{}, fmt.Errorf("resolving firewall, %w", err)
			}
			// Collected rather than returned on the first failure, so one pass
			// names every broken selector. Spec order is the iteration order,
			// so the message is byte-identical between identical reconciles.
			//
			// The most severe cause is kept, not the most recent: a rejected
			// credential alongside a genuinely missing firewall would otherwise
			// be reported as merely missing, depending on which was listed
			// last, and the operator would go looking for a deleted firewall
			// instead of a dead token. This path fails closed either way, so
			// unlike the ssh key equivalent only the message is at stake.
			if reason == "" || (r != reasonNotFound && reason == reasonNotFound) {
				reason, cause = r, err
			}
			missing = append(missing, describeSelector(sel.Name, sel.ID))
			continue
		}
		resolved = append(resolved, v1alpha1.FirewallStatus{ID: fw.ID, Name: fw.Name})
	}

	if len(missing) > 0 {
		// Fails closed: a partially resolved firewall set is published as no
		// set at all, because launching with a subset of the intended firewalls
		// silently opens whatever the missing one was there to close.
		//
		// The underlying error is carried into the message, without which a
		// missing firewall and a rejected token produce byte-identical text
		// under two reasons that differ only by a suffix.
		nodeClass.Status.Firewalls = nil
		nodeClass.StatusConditions(status.WithClock(f.clk)).SetFalse(
			v1alpha1.ConditionTypeFirewallsReady, "Firewall"+reason,
			failureMessage("firewallSelectors "+strings.Join(missing, ", "), reason, cause),
		)
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	nodeClass.Status.Firewalls = resolved
	nodeClass.StatusConditions(status.WithClock(f.clk)).SetTrue(v1alpha1.ConditionTypeFirewallsReady)
	return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
}
