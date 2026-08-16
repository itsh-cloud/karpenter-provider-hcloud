package main

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/bootstrap"
)

// Read-only debug subcommands. They exist so that the parts of the provider
// that are hardest to reason about, the discovered join parameters and the
// rendered cloud-init, can be inspected and diffed before anything provisions
// a node.
func runDebug(cmd string, args []string) (bool, error) {
	switch cmd {
	case "discover":
		return true, runDiscover()
	case "render-userdata":
		return true, runRenderUserData(args)
	default:
		return false, nil
	}
}

func newReadOnlyClient() (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

// runDiscover prints what the provider would use to join a node, so it can be
// compared against what the cluster's existing bootstrap actually uses.
func runDiscover() error {
	c, err := newReadOnlyClient()
	if err != nil {
		return err
	}

	d := bootstrap.NewDiscovery(c)
	if err := d.Refresh(context.Background()); err != nil {
		return err
	}
	endpoint, hashes, ok := d.Get()
	if !ok {
		return fmt.Errorf("discovery returned nothing usable")
	}

	fmt.Printf("apiServerEndpoint: %s\n", endpoint)
	for _, h := range hashes {
		fmt.Printf("caCertHash:        %s  (valid=%v)\n", h, bootstrap.ValidHash(h))
	}
	return nil
}

// runRenderUserData prints the cloud-init a node would receive, using a
// placeholder token so the output is safe to paste into a diff.
func runRenderUserData(args []string) error {
	version := "1.34.7"
	if len(args) > 0 && args[0] != "" {
		version = args[0]
	}

	endpoint, hashes := "10.1.0.2:6443", []string{"sha256:" + fmt.Sprintf("%064d", 0)}
	if c, err := newReadOnlyClient(); err == nil {
		d := bootstrap.NewDiscovery(c)
		if err := d.Refresh(context.Background()); err == nil {
			if e, h, ok := d.Get(); ok {
				endpoint, hashes = e, h
			}
		}
	}

	nc := &v1alpha1.HCloudNodeClass{
		Spec: v1alpha1.HCloudNodeClassSpec{
			Bootstrap: v1alpha1.BootstrapSpec{
				OSFamily:          v1alpha1.OSFamilyDebian,
				KubernetesVersion: version,
			},
		},
	}

	out, err := bootstrap.Render(bootstrap.Input{
		NodeClass: nc,
		Join: bootstrap.JoinInput{
			APIServerEndpoint: endpoint,
			CACertHashes:      hashes,
			// Deliberately not a real token: this output is for diffing.
			Token:      "aaaaaa.bbbbbbbbbbbbbbbb",
			NodeLabels: map[string]string{"karpenter.sh/nodepool": "example"},
			Kubelet:    &nc.Spec.Kubelet,
		},
	})
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
