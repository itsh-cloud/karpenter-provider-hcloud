package instance

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// commandReset is Hetzner's name for the one action Reboot issues.
const commandReset = "reset_server"

// heldDown holds the actions this provider never issues, so one on a managed
// server was run by someone who wants it off or in rescue.
var heldDown = map[string]bool{"stop_server": true, "shutdown_server": true, "enable_rescue": true}

// Reboot hard-resets the server behind a provider id, unless a reset already
// started at or after requestedAt.
//
// Like Delete, the ownership gate reads live state rather than the list cache,
// and the server must also be the NodeClaim's own. A server that is off, or
// that someone stopped or put into rescue, is refused rather than brought back,
// since that may be an operator containing it. A refusal is terminal, so core
// replaces the node and deletes its server.
func (p *Provider) Reboot(ctx context.Context, providerID, nodeClaimName string, requestedAt time.Time) error {
	id, err := hcloudapi.ServerIDFromProviderID(providerID)
	if err != nil {
		return cloudprovider.NewNodeRebootFailedError(err)
	}
	srv, err := p.servers.Get(ctx, id)
	if err != nil {
		return rebootError(err)
	}
	if srv == nil {
		return &hcloudapi.NotFoundError{Kind: "server", Selector: providerID}
	}
	if !srv.IsManagedBy(p.clusterName) {
		return cloudprovider.NewNodeRebootFailedError(errNotManaged{name: srv.Name, id: srv.ID})
	}
	if srv.Labels[hcloudapi.LabelNodeClaim] != nodeClaimName {
		return cloudprovider.NewNodeRebootFailedError(fmt.Errorf(
			"server %q (id %d) does not belong to nodeclaim %q; refusing to reset it", srv.Name, srv.ID, nodeClaimName))
	}

	actions, err := p.servers.RecentActions(ctx, id)
	if err != nil {
		return rebootError(err)
	}
	// Core retries with the same operation until this returns nil, so a reset
	// from an earlier call counts as issued. hcloud-go's own retries inside
	// one Reset call are not visible here.
	issued := false
	for _, a := range actions {
		switch {
		case a.Status == "error":
		case heldDown[a.Command]:
			return cloudprovider.NewNodeRebootFailedError(fmt.Errorf(
				"server %q (id %d) has a %s action karpenter did not issue; refusing to reset it", srv.Name, srv.ID, a.Command))
		case a.Command == commandReset && !requestedAt.IsZero() && !a.Started.Before(requestedAt):
			issued = true
		}
	}
	if issued {
		log.FromContext(ctx).Info("reset already issued", "server", srv.Name, "id", srv.ID)
		return nil
	}
	if srv.Status == "off" {
		return cloudprovider.NewNodeRebootFailedError(fmt.Errorf(
			"server %q (id %d) is off; refusing to power it on", srv.Name, srv.ID))
	}
	log.FromContext(ctx).Info("resetting server", "server", srv.Name, "id", srv.ID)
	if err := p.servers.Reset(ctx, id); err != nil {
		return rebootError(err)
	}
	return nil
}

// rebootError makes a rejected credential terminal, since retrying cannot fix
// it, and leaves everything else to core's retry.
func rebootError(err error) error {
	if hcloudapi.Classify(err) == hcloudapi.ClassFatal {
		return cloudprovider.NewNodeRebootFailedError(err)
	}
	return err
}
