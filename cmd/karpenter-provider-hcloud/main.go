// Command karpenter-provider-hcloud runs the Karpenter cloud provider for
// Hetzner Cloud.
//
// Provisioning, consolidation, drift handling and disruption all live in
// sigs.k8s.io/karpenter core. This binary supplies the CloudProvider
// implementation and the HCloudNodeClass controllers around it.
package main

import (
	"fmt"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/karpenter/pkg/operator"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/controllers"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/bootstrap"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/catalog"
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

	op.WithControllers(ctx, controllers.NewControllers(
		op.Clock,
		op.GetClient(),
		op.EventRecorder,
		// operatorpkg's generic status controller wants the raw client-go
		// recorder, which karpenter's deduplicating one does not implement.
		op.GetEventRecorderFor("karpenter-provider-hcloud"),
		hcloudapi.NewResources(hcloudClient),
		catalogProvider,
		// Uncached and direct, which is not interchangeable with GetClient:
		// see NewDiscoveryFromManager.
		bootstrap.NewDiscoveryFromManager(op.Manager),
	)...).Start(ctx)
}
