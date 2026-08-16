package hcloudapi

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Resolved identifiers for the things a NodeClass points at by name.
type (
	// Image is a resolved base image.
	Image struct {
		ID           int64
		Name         string
		Description  string
		Architecture string
		Created      string
	}

	// Network is a resolved private network.
	Network struct {
		ID      int64
		Name    string
		IPRange string
		Zone    string
	}

	// Firewall is a resolved firewall.
	Firewall struct {
		ID   int64
		Name string
	}

	// SSHKey is a resolved SSH key.
	SSHKey struct {
		ID          int64
		Name        string
		Fingerprint string
	}

	// PlacementGroup is a resolved placement group.
	PlacementGroup struct {
		ID   int64
		Name string
		Type string
		// ServerCount is current membership. Hetzner caps spread groups at 10
		// and the next create then fails with placement_error, which is
		// indistinguishable from a stockout and cannot be worked around by
		// choosing a different server type. Surfaced so the ceiling is visible
		// before it is hit.
		ServerCount int
	}
)

// NotFoundError reports a selector that resolved to nothing.
//
// Distinguished from a transport error because the reactions differ: a missing
// resource is a configuration problem to surface on the NodeClass, while a
// failed call should simply be retried.
type NotFoundError struct {
	Kind     string
	Selector string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found", e.Kind, e.Selector)
}

// Resources resolves the objects a NodeClass references.
type Resources interface {
	Image(ctx context.Context, name string, id *int64, arch string) (*Image, error)
	Network(ctx context.Context, name string, id *int64) (*Network, error)
	Firewall(ctx context.Context, name string, id *int64) (*Firewall, error)
	SSHKey(ctx context.Context, name string, id *int64) (*SSHKey, error)
	PlacementGroup(ctx context.Context, name string, id *int64) (*PlacementGroup, error)
}

type resourceClient struct{ c *hcloud.Client }

// NewResources returns a Resources backed by the given hcloud client.
func NewResources(c *hcloud.Client) Resources { return &resourceClient{c: c} }

// lookup resolves one name-or-id selector through a resource's pair of hcloud
// getters.
//
// Written once and shared because hcloud-go reports a miss as (nil, nil, nil),
// which every caller has to translate into a NotFoundError: the sub-reconcilers
// key the whole transient-versus-configuration split off that type, so a kind
// that forgot the translation would retry a missing resource forever instead of
// reporting it on the NodeClass.
func lookup[T any](
	ctx context.Context,
	kind, name string,
	id *int64,
	byID func(context.Context, int64) (*T, *hcloud.Response, error),
	byName func(context.Context, string) (*T, *hcloud.Response, error),
) (*T, error) {
	var (
		found *T
		err   error
		sel   string
	)
	if id != nil {
		sel = strconv.FormatInt(*id, 10)
		found, _, err = byID(ctx, *id)
	} else {
		sel = name
		found, _, err = byName(ctx, name)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving %s %q: %w", kind, sel, err)
	}
	if found == nil {
		return nil, &NotFoundError{Kind: kind, Selector: sel}
	}
	return found, nil
}

func (r *resourceClient) Image(ctx context.Context, name string, id *int64, arch string) (*Image, error) {
	hcloudArch := hcloud.ArchitectureX86
	if arch == "arm" {
		hcloudArch = hcloud.ArchitectureARM
	}
	// Name lookups must be architecture-qualified: the same image name exists
	// per architecture with different IDs, and picking the wrong one produces a
	// server that cannot boot.
	byName := func(ctx context.Context, name string) (*hcloud.Image, *hcloud.Response, error) {
		return r.c.Image.GetByNameAndArchitecture(ctx, name, hcloudArch)
	}
	img, err := lookup(ctx, "image", name, id, r.c.Image.GetByID, byName)
	if err != nil {
		return nil, err
	}
	return &Image{
		ID:           img.ID,
		Name:         img.Name,
		Description:  img.Description,
		Architecture: string(img.Architecture),
		Created:      img.Created.UTC().Format("2006-01-02T15:04:05Z"),
	}, nil
}

func (r *resourceClient) Network(ctx context.Context, name string, id *int64) (*Network, error) {
	n, err := lookup(ctx, "network", name, id, r.c.Network.GetByID, r.c.Network.GetByName)
	if err != nil {
		return nil, err
	}
	out := &Network{ID: n.ID, Name: n.Name}
	if n.IPRange != nil {
		out.IPRange = n.IPRange.String()
	}
	// A network's zone bounds which locations can attach to it, so a NodeClass
	// cannot offer a location outside it however its selectors are written.
	for _, sub := range n.Subnets {
		if sub.NetworkZone != "" {
			out.Zone = string(sub.NetworkZone)
			break
		}
	}
	return out, nil
}

func (r *resourceClient) Firewall(ctx context.Context, name string, id *int64) (*Firewall, error) {
	f, err := lookup(ctx, "firewall", name, id, r.c.Firewall.GetByID, r.c.Firewall.GetByName)
	if err != nil {
		return nil, err
	}
	return &Firewall{ID: f.ID, Name: f.Name}, nil
}

func (r *resourceClient) SSHKey(ctx context.Context, name string, id *int64) (*SSHKey, error) {
	k, err := lookup(ctx, "ssh key", name, id, r.c.SSHKey.GetByID, r.c.SSHKey.GetByName)
	if err != nil {
		return nil, err
	}
	return &SSHKey{ID: k.ID, Name: k.Name, Fingerprint: k.Fingerprint}, nil
}

func (r *resourceClient) PlacementGroup(ctx context.Context, name string, id *int64) (*PlacementGroup, error) {
	g, err := lookup(ctx, "placement group", name, id, r.c.PlacementGroup.GetByID, r.c.PlacementGroup.GetByName)
	if err != nil {
		return nil, err
	}
	return &PlacementGroup{
		ID:          g.ID,
		Name:        g.Name,
		Type:        string(g.Type),
		ServerCount: len(g.Servers),
	}, nil
}
