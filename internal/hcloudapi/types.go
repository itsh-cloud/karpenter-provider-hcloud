package hcloudapi

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// The types below are this provider's own view of the Hetzner catalog. They
// exist so that only this package imports hcloud-go, which keeps the rest of
// the provider testable without a fake HTTP server and makes an upstream API
// change a one-package problem.

// Price is the net cost of one server type in one location.
type Price struct {
	HourlyNet  float64
	MonthlyNet float64
}

// ServerType is one Hetzner machine shape.
type ServerType struct {
	ID   int64
	Name string

	Cores int
	// MemoryGiB is what Hetzner advertises. The machine reports several
	// percent less; see the instancetype package for the correction.
	MemoryGiB float64
	// DiskGB is DECIMAL GB, not GiB.
	DiskGB int

	Architecture string // "x86" or "arm"
	CPUType      string // "shared" or "dedicated"
	StorageType  string // "local" or "network"
	Deprecated   bool

	// Prices is keyed by location name. A location absent here does not offer
	// this type at all, which is a stronger statement than the availability
	// flag and is the check worth making.
	Prices map[string]Price

	// Locations is where this type is offered, and is the successor to the
	// deprecated Datacenter.ServerTypes listing.
	//
	// Note the granularity change: the old API reported per DATACENTER
	// (nbg1-dc3), the new one reports per LOCATION (nbg1). That is why
	// offerings constrain topology.kubernetes.io/region and leave zone free:
	// we can no longer enumerate datacenters, and we do not need to, since
	// servers are ordered by location and hcloud-CCM labels the node with
	// whichever datacenter it landed in.
	Locations []ServerTypeLocation
}

// ServerTypeLocation is one location a server type is offered in.
type ServerTypeLocation struct {
	Location    string
	NetworkZone string

	// Available is Hetzner's published stock hint.
	//
	// ADVISORY ONLY, and deliberately not used to gate provisioning. Measured
	// against the live API on 2026-08-16, cx43 and cx53 were both reported
	// available:false for nbg1 and ordering them there succeeded anyway
	// (HTTP 201, create_server action reaching success), reproduced three
	// times. It is not sufficient either: types reported available have
	// returned resource_unavailable. Upstream cluster-autoscaler's Hetzner
	// provider does not consult it at all.
	//
	// Gating on it would exclude exactly the cheap types worth returning to
	// after a stockout. Export it as a metric; decide nothing with it.
	Available bool

	// Recommended is Hetzner's own preference hint. Unused, exported for
	// observability.
	Recommended bool

	// Deprecated marks a type being phased out in this specific location.
	Deprecated bool
}

// Line returns the Hetzner product line prefix: cx, cpx, ccx or cax.
func (s ServerType) Line() string {
	for _, prefix := range []string{"ccx", "cpx", "cax", "cx"} {
		if len(s.Name) >= len(prefix) && s.Name[:len(prefix)] == prefix {
			return prefix
		}
	}
	return ""
}

// Catalog is the read-only slice of the Hetzner API this provider needs to
// build its instance-type catalog.
type Catalog interface {
	ServerTypes(ctx context.Context) ([]ServerType, error)
	// PrimaryIPv4MonthlyNet is billed separately from the server and is
	// included in an offering's price when the NodeClass requests a public
	// IPv4.
	PrimaryIPv4MonthlyNet(ctx context.Context) (float64, error)
}

// catalogClient adapts an hcloud.Client to Catalog.
type catalogClient struct{ c *hcloud.Client }

// NewCatalog returns a Catalog backed by the given hcloud client.
func NewCatalog(c *hcloud.Client) Catalog { return &catalogClient{c: c} }

func (a *catalogClient) ServerTypes(ctx context.Context) ([]ServerType, error) {
	sts, err := a.c.ServerType.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing server types: %w", err)
	}

	out := make([]ServerType, 0, len(sts))
	for _, st := range sts {
		prices := make(map[string]Price, len(st.Pricings))
		for _, p := range st.Pricings {
			if p.Location == nil {
				continue
			}
			hourly, err1 := strconv.ParseFloat(p.Hourly.Net, 64)
			monthly, err2 := strconv.ParseFloat(p.Monthly.Net, 64)
			if err1 != nil || err2 != nil {
				// A price we cannot parse must not silently become zero: a
				// free-looking offering would win every comparison and the
				// fleet would converge on it.
				continue
			}
			prices[p.Location.Name] = Price{HourlyNet: hourly, MonthlyNet: monthly}
		}

		locs := make([]ServerTypeLocation, 0, len(st.Locations))
		for _, l := range st.Locations {
			if l.Location == nil {
				continue
			}
			locs = append(locs, ServerTypeLocation{
				Location:    l.Location.Name,
				NetworkZone: string(l.Location.NetworkZone),
				Available:   l.Available,
				Recommended: l.Recommended,
				Deprecated:  l.IsDeprecated(),
			})
		}

		out = append(out, ServerType{
			ID:           st.ID,
			Name:         st.Name,
			Cores:        st.Cores,
			MemoryGiB:    float64(st.Memory),
			DiskGB:       st.Disk,
			Architecture: string(st.Architecture),
			CPUType:      string(st.CPUType),
			StorageType:  string(st.StorageType),
			Deprecated:   st.IsDeprecated(),
			Prices:       prices,
			Locations:    locs,
		})
	}
	return out, nil
}

func (a *catalogClient) PrimaryIPv4MonthlyNet(ctx context.Context) (float64, error) {
	pricing, _, err := a.c.Pricing.Get(ctx)
	if err != nil {
		return 0, fmt.Errorf("getting pricing: %w", err)
	}
	for _, p := range pricing.PrimaryIPs {
		if p.Type != "ipv4" {
			continue
		}
		for _, price := range p.Pricings {
			v, err := strconv.ParseFloat(price.Monthly.Net, 64)
			if err != nil {
				continue
			}
			return v, nil
		}
	}
	// Not fatal: a missing surcharge only makes offerings look marginally
	// cheaper than they are, uniformly, so relative ordering is unaffected.
	return 0, nil
}
