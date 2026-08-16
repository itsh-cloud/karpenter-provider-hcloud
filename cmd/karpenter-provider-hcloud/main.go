// Command karpenter-provider-hcloud runs the Karpenter cloud provider for
// Hetzner Cloud.
//
// Provisioning, consolidation, drift handling and disruption all live in
// sigs.k8s.io/karpenter core. This binary supplies the CloudProvider
// implementation and the HCloudNodeClass controllers around it.
package main

import (
	"context"
	"fmt"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/karpenter/pkg/operator"

	corecontrollers "sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"

	hcloudprovider "github.com/itsh-cloud/karpenter-provider-hcloud/internal/cloudprovider"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/controllers"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/metrics"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/bootstrap"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/catalog"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instance"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instancetype"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func init() { hcloudapi.Version = version }

func main() {
	// Handled before operator.NewOperator, which parses its own flag set and
	// exits on an unknown flag. The operator owns --metrics-port (8080),
	// --health-probe-port (8081), --feature-gates and the rest.
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-version" {
			fmt.Println(version)
			return
		}
	}

	// Read-only debug subcommands, for inspecting what the provider would do
	// before it does it.
	if len(os.Args) > 1 {
		if handled, err := runDebug(os.Args[1], os.Args[2:]); handled {
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
	}

	ctx, op := operator.NewOperator()

	hcloudClient, err := hcloudapi.NewClientFromEnv()
	if err != nil {
		log.FromContext(ctx).Error(err, "building the Hetzner API client")
		os.Exit(1)
	}

	catalogProvider := catalog.NewProvider(hcloudapi.NewCatalog(hcloudClient))
	// Added as a Runnable rather than started here, so the refresh loop shuts
	// down with the manager and only runs on the elected leader.
	if err := op.Add(manager.RunnableFunc(catalogProvider.Start)); err != nil {
		log.FromContext(ctx).Error(err, "registering the catalog refresh loop")
		os.Exit(1)
	}

	clusterName := os.Getenv(clusterNameEnvVar)
	if clusterName == "" {
		// Fatal, not defaulted. This value becomes the karpenter.sh/managed-by
		// label on every server, and that label is the ownership check every
		// destructive path gates on. Defaulting it would let two clusters
		// sharing one Hetzner project delete each other's nodes, and would make
		// "is this ours?" answerable by accident.
		log.FromContext(ctx).Error(nil, "refusing to start", "reason", clusterNameEnvVar+" is not set")
		os.Exit(1)
	}

	discovery := bootstrap.NewDiscoveryFromManager(op.Manager)
	unavailable := instancetype.NewUnavailable()

	// Suppressions expire on a timer with nothing to hook, so the gauge is
	// rebuilt periodically rather than only written on failure. Without it a
	// pair reads as permanently out of stock long after it recovered.
	if err := op.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return metrics.SyncUnavailable(ctx, unavailable)
	})); err != nil {
		log.FromContext(ctx).Error(err, "registering the offering availability metric sync")
		os.Exit(1)
	}

	instanceProvider := instance.NewProvider(hcloudapi.NewServers(hcloudClient), unavailable, clusterName)

	cloudProvider := hcloudprovider.New(
		op.GetClient(),
		instanceProvider,
		catalogProvider,
		unavailable,
		bootstrap.NewProvider(bootstrap.NewTokenMinter(op.GetClient()), discovery, clusterName),
		clusterName,
	)

	cluster := state.NewCluster(op.Clock, op.GetClient(), cloudProvider)

	op.WithControllers(ctx, controllers.NewControllers(
		op.Clock,
		op.GetClient(),
		op.EventRecorder,
		// operatorpkg's generic status controller wants the raw client-go
		// recorder, which karpenter's deduplicating one does not implement.
		//
		// The deprecated call is unavoidable here, not an oversight.
		// GetEventRecorder returns the newer events.EventRecorder, while
		// operatorpkg's status.NewController takes a record.EventRecorder, so
		// the migration is gated on operatorpkg rather than on us. Karpenter
		// core carries the same suppression at pkg/operator/operator.go for
		// the same reason.
		op.GetEventRecorderFor("karpenter-provider-hcloud"), //nolint:staticcheck // SA1019: blocked on operatorpkg taking record.EventRecorder
		hcloudapi.NewResources(hcloudClient),
		catalogProvider,
		// Uncached and direct, which is not interchangeable with GetClient:
		// see NewDiscoveryFromManager.
		discovery,
		instanceProvider,
		clusterName,
	)...).WithControllers(ctx, corecontrollers.NewControllers(
		ctx,
		op.Manager,
		op.Clock,
		op.GetClient(),
		op.EventRecorder,
		cloudProvider,
		// The undecorated provider is the same one: node overlays are not
		// enabled, so there is no decoration to see past.
		cloudProvider,
		cluster,
		op.InstanceTypeStore,
	)...).Start(ctx)
}

// clusterNameEnvVar names the cluster, and therefore what this controller owns.
const clusterNameEnvVar = "CLUSTER_NAME"
