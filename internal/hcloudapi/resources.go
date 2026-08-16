package hcloudapi

import (
	"context"
	"fmt"

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

func (r *resourceClient) Image(ctx context.Context, name string, id *int64, arch string) (*Image, error) {
	var (
		img *hcloud.Image
		err error
		sel string
	)
	switch {
	case id != nil:
		sel = fmt.Sprint(*id)
		img, _, err = r.c.Image.GetByID(ctx, *id)
	default:
		sel = name
		a := hcloud.ArchitectureX86
		if arch == "arm" {
			a = hcloud.ArchitectureARM
		}
		// Name lookups must be architecture-qualified: the same image name
		// exists per architecture with different IDs, and picking the wrong one
		// produces a server that cannot boot.
		img, _, err = r.c.Image.GetByNameAndArchitecture(ctx, name, a)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving image %q: %w", sel, err)
	}
	if img == nil {
		return nil, &NotFoundError{Kind: "image", Selector: sel}
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
	var (
		n   *hcloud.Network
		err error
		sel string
	)
	if id != nil {
		sel = fmt.Sprint(*id)
		n, _, err = r.c.Network.GetByID(ctx, *id)
	} else {
		sel = name
		n, _, err = r.c.Network.GetByName(ctx, name)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving network %q: %w", sel, err)
	}
	if n == nil {
		return nil, &NotFoundError{Kind: "network", Selector: sel}
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
	var (
		f   *hcloud.Firewall
		err error
		sel string
	)
	if id != nil {
		sel = fmt.Sprint(*id)
		f, _, err = r.c.Firewall.GetByID(ctx, *id)
	} else {
		sel = name
		f, _, err = r.c.Firewall.GetByName(ctx, name)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving firewall %q: %w", sel, err)
	}
	if f == nil {
		return nil, &NotFoundError{Kind: "firewall", Selector: sel}
	}
	return &Firewall{ID: f.ID, Name: f.Name}, nil
}

func (r *resourceClient) SSHKey(ctx context.Context, name string, id *int64) (*SSHKey, error) {
	var (
		k   *hcloud.SSHKey
		err error
		sel string
	)
	if id != nil {
		sel = fmt.Sprint(*id)
		k, _, err = r.c.SSHKey.GetByID(ctx, *id)
	} else {
		sel = name
		k, _, err = r.c.SSHKey.GetByName(ctx, name)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving ssh key %q: %w", sel, err)
	}
	if k == nil {
		return nil, &NotFoundError{Kind: "ssh key", Selector: sel}
	}
	return &SSHKey{ID: k.ID, Name: k.Name, Fingerprint: k.Fingerprint}, nil
}

func (r *resourceClient) PlacementGroup(ctx context.Context, name string, id *int64) (*PlacementGroup, error) {
	var (
		g   *hcloud.PlacementGroup
		err error
		sel string
	)
	if id != nil {
		sel = fmt.Sprint(*id)
		g, _, err = r.c.PlacementGroup.GetByID(ctx, *id)
	} else {
		sel = name
		g, _, err = r.c.PlacementGroup.GetByName(ctx, name)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving placement group %q: %w", sel, err)
	}
	if g == nil {
		return nil, &NotFoundError{Kind: "placement group", Selector: sel}
	}
	return &PlacementGroup{
		ID:          g.ID,
		Name:        g.Name,
		Type:        string(g.Type),
		ServerCount: len(g.Servers),
	}, nil
}
