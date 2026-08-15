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

	"sigs.k8s.io/karpenter/pkg/operator"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

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

	ctx, op := operator.NewOperator()

	// No controllers registered yet. The operator still serves /metrics and
	// the health probes, acquires leader election and starts its manager,
	// which is what this phase needs to demonstrate.
	op.Start(ctx)
}
