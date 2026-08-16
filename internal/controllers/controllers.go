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
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/controllers/nodeclass"
	nodeclasshash "github.com/itsh-cloud/karpenter-provider-hcloud/internal/controllers/nodeclass/hash"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/bootstrap"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/catalog"
)

// The controllers take narrowed interfaces so they can be tested without a
// refresh loop or a live cluster. These assertions keep the narrowing honest:
// without them, a signature change in either provider would only be caught at
// the call site in main, which the tests never reach.
var (
	_ nodeclass.CatalogProvider = (*catalog.Provider)(nil)
	_ nodeclass.Discovery       = (*bootstrap.Discovery)(nil)

	// The status controller registered below is generic over status.Object, so
	// without this the first thing to notice a drifted GetConditions or
	// StatusConditions signature is the compiler at that call site, and the
	// second is core's readiness controller at runtime. status.ForOption is
	// exactly the shape that shifts under a dependency bump.
	_ status.Object = (*v1alpha1.HCloudNodeClass)(nil)
)

// NewControllers returns every controller this provider registers, in the shape
// karpenter's operator.WithControllers takes.
//
// Two event recorders, and they are not interchangeable. Karpenter's
// events.Recorder deduplicates, which is what the termination and
// location-narrowing events need since both re-fire on a timer. operatorpkg's
// status controller wants the raw client-go recorder.
func NewControllers(
	clk clock.Clock,
	kubeClient client.Client,
	recorder events.Recorder,
	eventRecorder record.EventRecorder,
	resources hcloudapi.Resources,
	catalogProvider nodeclass.CatalogProvider,
	discovery nodeclass.Discovery,
) []controller.Controller {
	return []controller.Controller{
		nodeclasshash.NewController(kubeClient),
		nodeclass.NewController(clk, kubeClient, recorder, resources, catalogProvider, discovery),
		// Observability only: this emits condition metrics and a Kubernetes
		// event on every condition transition. It never writes to the API, so
		// its unconditional ten-second requeue is a metric refresh rather than
		// status churn. Registered here because karpenter core only registers
		// it for its own NodeClaim and NodePool types, so without this a
		// NodeClass stuck at Ready=False is invisible to alerting.
		status.NewController[*v1alpha1.HCloudNodeClass](kubeClient, eventRecorder),
	}
}
