package cloudprovider

import (
	"context"
	"testing"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
)

// Core only validates the policy set when NodeRepair is on, so a set it
// rejects stays hidden until the day someone enables the gate and the
// controller fails to start.
func TestRepairPoliciesPassCoreValidation(t *testing.T) {
	ctx := coreoptions.ToContext(context.Background(), &coreoptions.Options{
		FeatureGates: coreoptions.FeatureGates{NodeRepair: true},
	})
	cp := &CloudProvider{}

	if _, err := health.NewRepairPolicyMatcher(ctx, cp); err != nil {
		t.Fatalf("core rejects the repair policies: %v", err)
	}
	for i, p := range cp.RepairPolicies() {
		if p.Action != cloudprovider.ReplaceNode {
			t.Errorf("policy %d: action %q, want %q", i, p.Action, cloudprovider.ReplaceNode)
		}
		if p.TerminationGracePeriod == nil || *p.TerminationGracePeriod != 0 {
			t.Errorf("policy %d: termination grace period %v, want 0 (forceful)", i, p.TerminationGracePeriod)
		}
	}
}

func TestRebootIsNotImplemented(t *testing.T) {
	err := (&CloudProvider{}).Reboot(context.Background(), &karpv1.NodeClaim{}, "op")
	if !cloudprovider.IsNodeRebootNotImplementedError(err) {
		t.Fatalf("Reboot returned %v, want a NodeRebootNotImplementedError", err)
	}
}
