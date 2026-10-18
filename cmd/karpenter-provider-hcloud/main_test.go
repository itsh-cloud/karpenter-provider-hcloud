package main

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	hcloudprovider "github.com/itsh-cloud/karpenter-provider-hcloud/internal/cloudprovider"
)

// Core's repair disruption method panics on a nil matcher whenever NodeRepair
// is on, so the matcher has to reach cluster state at construction.
func TestClusterStateCarriesRepairMatcher(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nodeRepair  bool
		wantMatcher bool
	}{
		{name: "gate on", nodeRepair: true, wantMatcher: true},
		{name: "gate off", nodeRepair: false, wantMatcher: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := coreoptions.ToContext(context.Background(), &coreoptions.Options{
				FeatureGates: coreoptions.FeatureGates{NodeRepair: tc.nodeRepair},
			})
			cluster, err := newClusterState(ctx, clocktesting.NewFakeClock(time.Now()), fake.NewClientBuilder().Build(), &hcloudprovider.CloudProvider{})
			if err != nil {
				t.Fatalf("newClusterState: %v", err)
			}
			if got := cluster.RepairPolicyMatcher() != nil; got != tc.wantMatcher {
				t.Fatalf("matcher present = %v, want %v", got, tc.wantMatcher)
			}
		})
	}
}

func TestCloudProviderErrorsAreRecorded(t *testing.T) {
	nodeClaim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{karpv1.NodePoolLabelKey: "metrics-test"},
	}}
	if err := withMetrics(&hcloudprovider.CloudProvider{}).Reboot(context.Background(), nodeClaim, ""); err == nil {
		t.Fatal("Reboot succeeded, want not-implemented")
	}

	families, err := crmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "karpenter_cloudprovider_errors_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["method"] == "Reboot" && labels["nodepool"] == "metrics-test" && m.GetCounter().GetValue() > 0 {
				return
			}
		}
	}
	t.Fatal("no karpenter_cloudprovider_errors_total sample for the failed Reboot")
}
