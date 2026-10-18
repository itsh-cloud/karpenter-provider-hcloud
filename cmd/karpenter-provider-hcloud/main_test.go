package main

import (
	"context"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
