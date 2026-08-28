package nodeclass

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/bootstrap"
)

// Discovery is the slice of bootstrap.Discovery this reconciler needs.
type Discovery interface {
	Refresh(ctx context.Context) error
	Resolve(endpointOverride string, hashOverride []string) (string, []string, error)
}

// BootstrapDiscovery resolves the kubeadm join endpoint and CA pins into
// status, gating provisioning on them being known.
//
// Every other selector failing produces a create that errors; this one failing
// produces a server that boots, bills and never joins, replaced on karpenter's
// fifteen-minute registration timeout by another that does the same.
type BootstrapDiscovery struct {
	clk       clock.Clock
	discovery Discovery
}

// NewBootstrapDiscovery returns the bootstrap discovery sub-reconciler.
func NewBootstrapDiscovery(clk clock.Clock, discovery Discovery) *BootstrapDiscovery {
	return &BootstrapDiscovery{clk: clk, discovery: discovery}
}

func (b *BootstrapDiscovery) Reconcile(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	spec := nodeClass.Spec.Bootstrap
	conds := nodeClass.StatusConditions(status.WithClock(b.clk))

	// Every configuration failure below clears both join parameters, not just
	// the one it noticed: they are consumed as a pair, and half a pair is a
	// server that boots and never joins.
	misconfigured := func(reason, message string) (reconcile.Result, error) {
		nodeClass.Status.APIServerEndpoint = ""
		nodeClass.Status.CACertHashes = nil
		conds.SetFalse(v1alpha1.ConditionTypeBootstrapDiscoveryReady, reason, message)
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	// cluster-info is only read when it is actually needed: a NodeClass that
	// overrides both values is fully self-contained, and making it depend on a
	// ConfigMap it never consults would let an RBAC gap or a non-kubeadm
	// control plane break it.
	if spec.APIServerEndpoint == "" || len(spec.CACertHashes) == 0 {
		if err := b.discovery.Refresh(ctx); err != nil {
			var configErr *bootstrap.ConfigError
			if !errors.As(err, &configErr) {
				// Transient, treated like a Hetzner blip: condition untouched,
				// error returned for backoff. This is the one dependency read
				// over the network, so a control-plane rolling upgrade lands
				// here, and publishing that as False would take every NodeClass
				// to Ready=False and make core delete in-flight NodeClaims. A
				// class that NEVER resolved still gets a diagnosis rather than
				// sitting at "awaiting reconciliation".
				noteUnreachable(conds, v1alpha1.ConditionTypeBootstrapDiscoveryReady, "ClusterInfo", err)
				return reconcile.Result{}, fmt.Errorf("reading cluster-info, %w", err)
			}
			// Configuration: the control plane was not built with kubeadm, the
			// Role was not installed, or the embedded kubeconfig is malformed.
			// None are fixed by backoff, and all have to be readable off the
			// NodeClass rather than only in the logs.
			return misconfigured("ClusterInfoUnavailable",
				fmt.Sprintf("reading the join parameters from %s/%s: %s", bootstrap.ClusterInfoNamespace, bootstrap.ClusterInfoName, err))
		}
	}

	endpoint, hashes, err := b.discovery.Resolve(spec.APIServerEndpoint, spec.CACertHashes)
	if err != nil {
		return misconfigured("JoinParametersIncomplete", err.Error())
	}

	// Re-validated even when the pins came from cluster-info, because
	// spec.bootstrap.caCertHashes is a possible source and the CRD pattern only
	// constrains entries present in the manifest. kubeadm rejects a malformed
	// pin at join time with an error that reaches nobody.
	if bad := invalidHashes(hashes); len(bad) > 0 {
		return misconfigured("InvalidCACertHash",
			fmt.Sprintf("CA pins are not in sha256:<64 hex> form: %s", strings.Join(bad, ", ")))
	}

	nodeClass.Status.APIServerEndpoint = endpoint
	nodeClass.Status.CACertHashes = hashes
	conds.SetTrue(v1alpha1.ConditionTypeBootstrapDiscoveryReady)
	return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
}

// invalidHashes returns the pins that are not kubeadm-shaped, in input order so
// the message it feeds is stable across reconciles.
func invalidHashes(hashes []string) []string {
	var bad []string
	for _, h := range hashes {
		if !bootstrap.ValidHash(h) {
			bad = append(bad, h)
		}
	}
	return bad
}
