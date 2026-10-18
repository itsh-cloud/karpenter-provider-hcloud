package cloudprovider

import (
	"context"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
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
	var reboots []cloudprovider.RepairPolicy
	for i, p := range cp.RepairPolicies() {
		if p.TerminationGracePeriod == nil || *p.TerminationGracePeriod != 0 {
			t.Errorf("policy %d: termination grace period %v, want 0 (forceful)", i, p.TerminationGracePeriod)
		}
		if p.Action == cloudprovider.RebootNode {
			reboots = append(reboots, p)
		}
		if p.ReasonRegex == "" && p.Action != cloudprovider.ReplaceNode {
			t.Errorf("policy %d: the fallback's action is %q, want %q", i, p.Action, cloudprovider.ReplaceNode)
		}
	}
	if len(reboots) != 1 || reboots[0].ConditionType != corev1.NodeReady || reboots[0].ConditionStatus != corev1.ConditionUnknown {
		t.Errorf("reboot policies = %+v, want exactly one, on Ready=Unknown", reboots)
	}
}

// TestRepairPolicyLadder walks the policies through core's own matcher, so the
// order reset-then-replace is what core decides rather than what the policy
// list looks like.
func TestRepairPolicyLadder(t *testing.T) {
	ctx := coreoptions.ToContext(context.Background(), &coreoptions.Options{
		FeatureGates: coreoptions.FeatureGates{NodeRepair: true},
	})
	matcher, err := health.NewRepairPolicyMatcher(ctx, &CloudProvider{})
	if err != nil {
		t.Fatalf("NewRepairPolicyMatcher: %v", err)
	}

	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(15 * time.Minute)
	node := func(status corev1.ConditionStatus, reason string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "n1", CreationTimestamp: metav1.NewTime(t0.Add(-time.Hour))},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeReady, Status: status, Reason: reason, LastTransitionTime: metav1.NewTime(t0),
			}}},
		}
	}
	silent := node(corev1.ConditionUnknown, "NodeStatusUnknown")
	notReady := node(corev1.ConditionFalse, "KubeletNotReady")

	for _, tc := range []struct {
		name      string
		node      *corev1.Node
		now       time.Time
		notBefore time.Time
		want      cloudprovider.RepairAction
	}{
		{"silentJustBeforeReset", silent, t0.Add(10*time.Minute - time.Second), time.Time{}, ""},
		{"silentIsReset", silent, t0.Add(10 * time.Minute), time.Time{}, cloudprovider.RebootNode},
		{"silentIsReplaced", silent, t0.Add(30 * time.Minute), time.Time{}, cloudprovider.ReplaceNode},
		{"notReadyIsNotReset", notReady, t0.Add(29 * time.Minute), time.Time{}, ""},
		{"notReadyIsReplaced", notReady, t0.Add(30 * time.Minute), time.Time{}, cloudprovider.ReplaceNode},
		{"notBeforeDelaysTheReset", silent, t1.Add(9 * time.Minute), t1, ""},
		{"notBeforeThenReset", silent, t1.Add(10 * time.Minute), t1, cloudprovider.RebootNode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := health.Resolve(matcher.Match(tc.node), tc.now, tc.notBefore).Action; got != tc.want {
				t.Errorf("action = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReboot: core replaces the node on NodeClaimNotFound and on
// NodeRebootFailed, and retries anything else with the same operation.
func TestReboot(t *testing.T) {
	requestedAt := testNow.Add(-5 * time.Minute)
	claim := func(providerID string) *karpv1.NodeClaim {
		return &karpv1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "n1"},
			Status: karpv1.NodeClaimStatus{ProviderID: providerID, Conditions: []status.Condition{{
				Type: karpv1.ConditionTypeRebooting, Status: metav1.ConditionTrue, Reason: karpv1.RebootReasonRequested,
				LastTransitionTime: metav1.NewTime(requestedAt),
			}}},
		}
	}

	t.Run("emptyProviderIDIsNotFound", func(t *testing.T) {
		cp, _ := newTestProvider(t)
		if err := cp.Reboot(context.Background(), claim(""), "op"); !cloudprovider.IsNodeClaimNotFoundError(err) {
			t.Errorf("err = %v, want NodeClaimNotFoundError", err)
		}
	})
	t.Run("missingServerIsNotFound", func(t *testing.T) {
		cp, _ := newTestProvider(t)
		if err := cp.Reboot(context.Background(), claim("hcloud://7"), "op"); !cloudprovider.IsNodeClaimNotFoundError(err) {
			t.Errorf("err = %v, want NodeClaimNotFoundError", err)
		}
	})
	t.Run("unmanagedServerIsRebootFailed", func(t *testing.T) {
		cp, servers := newTestProvider(t)
		servers.byID[7] = &hcloudapi.Server{ID: 7, Name: "k8s-node-1"}
		if err := cp.Reboot(context.Background(), claim("hcloud://7"), "op"); !cloudprovider.IsNodeRebootFailedError(err) {
			t.Errorf("err = %v, want NodeRebootFailedError", err)
		}
		if len(servers.resets) != 0 {
			t.Errorf("reset a server this cluster does not own: %v", servers.resets)
		}
	})
	t.Run("transientErrorIsRetried", func(t *testing.T) {
		cp, servers := newTestProvider(t)
		servers.getErr = errTransient
		err := cp.Reboot(context.Background(), claim("hcloud://7"), "op")
		if err == nil || cloudprovider.IsNodeClaimNotFoundError(err) || cloudprovider.IsNodeRebootFailedError(err) {
			t.Errorf("err = %v, want a plain error core retries", err)
		}
	})
	// The Rebooting condition's transition time is the request time the
	// dedupe compares against.
	for _, tc := range []struct {
		name       string
		started    time.Time
		wantResets int
	}{
		{"resetSinceTheRequestIsNotRepeated", requestedAt.Add(time.Minute), 0},
		{"resetBeforeTheRequestIsRepeated", requestedAt.Add(-time.Minute), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, servers := newTestProvider(t)
			servers.byID[7] = &hcloudapi.Server{ID: 7, Name: "worker", Status: "running",
				Labels: map[string]string{hcloudapi.LabelManagedBy: testCluster, hcloudapi.LabelNodeClaim: "n1"}}
			servers.actions = []hcloudapi.Action{{Command: "reset_server", Status: "success", Started: tc.started}}
			if err := cp.Reboot(context.Background(), claim("hcloud://7"), "op"); err != nil {
				t.Fatalf("Reboot: %v", err)
			}
			if len(servers.resets) != tc.wantResets {
				t.Errorf("resets = %v, want %d", servers.resets, tc.wantResets)
			}
		})
	}
}
