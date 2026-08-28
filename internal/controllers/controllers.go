// Package controllers assembles the provider's controllers for
// operator.WithControllers.
package controllers

import (
	"github.com/awslabs/operatorpkg/controller"
	"github.com/awslabs/operatorpkg/status"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/controllers/instancegc"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/controllers/nodeclass"
	nodeclasshash "github.com/itsh-cloud/karpenter-provider-hcloud/internal/controllers/nodeclass/hash"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/bootstrap"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/catalog"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instance"
)

// The controllers take narrowed interfaces so they can be tested without a
// refresh loop or a live cluster. These assertions keep the narrowing honest: a
// signature change would otherwise only be caught in main, which tests never
// reach.
var (
	_ nodeclass.CatalogProvider = (*catalog.Provider)(nil)
	_ nodeclass.Discovery       = (*bootstrap.Discovery)(nil)
	_ instancegc.Provider       = (*instance.Provider)(nil)

	// The status controller below is generic over status.Object, and
	// status.ForOption is exactly the shape that shifts under a dependency
	// bump. Without this, a drifted signature surfaces at runtime in core's
	// readiness controller.
	_ status.Object = (*v1alpha1.HCloudNodeClass)(nil)
)

// NewControllers returns every controller this provider registers, in the shape
// karpenter's operator.WithControllers takes.
//
// Two event recorders, not interchangeable: karpenter's events.Recorder
// deduplicates, which the termination and location-narrowing events need since
// both re-fire on a timer, while operatorpkg's status controller wants the raw
// client-go recorder.
func NewControllers(
	clk clock.Clock,
	kubeClient client.Client,
	recorder events.Recorder,
	eventRecorder record.EventRecorder,
	resources hcloudapi.Resources,
	catalogProvider nodeclass.CatalogProvider,
	discovery nodeclass.Discovery,
	instances instancegc.Provider,
	clusterName string,
) []controller.Controller {
	return []controller.Controller{
		nodeclasshash.NewController(kubeClient),
		nodeclass.NewController(clk, kubeClient, recorder, resources, catalogProvider, discovery),
		// Core reaps NodeClaims whose instance is gone, never the reverse, so
		// this side is the provider's to ship.
		instancegc.NewController(clk, kubeClient, instances, clusterName),
		// Observability only: condition metrics and an event per transition,
		// never a write to the API, so its ten-second requeue is a metric
		// refresh rather than status churn. Registered here because core only
		// registers it for NodeClaim and NodePool, so without this a NodeClass
		// stuck at Ready=False is invisible to alerting.
		status.NewController[*v1alpha1.HCloudNodeClass](kubeClient, eventRecorder),
	}
}
