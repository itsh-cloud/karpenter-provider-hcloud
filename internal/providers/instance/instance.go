// Package instance turns a scheduled NodeClaim into a running Hetzner server.
package instance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/metrics"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instancetype"
)

// DefaultMaxCreateAttempts bounds how many (type, location) candidates one
// Create walks before giving up.
//
// Each attempt costs a POST plus an action wait, so a project genuinely out of
// capacity everywhere would otherwise spend minutes and a large slice of the
// shared rate limit discovering that. Eight crosses several types and both
// locations.
const DefaultMaxCreateAttempts = 8

// Candidate is one (instance type, location) pair a NodeClaim could be
// satisfied by, at the price that pair is offered for.
type Candidate struct {
	InstanceType *cloudprovider.InstanceType
	Location     string
	Price        float64
}

// Provider creates, deletes and reads the servers backing NodeClaims.
type Provider struct {
	servers     hcloudapi.Servers
	unavailable *instancetype.Unavailable
	clusterName string
	maxAttempts int
	cache       *listCache
}

// NewProvider returns an instance provider.
func NewProvider(servers hcloudapi.Servers, unavailable *instancetype.Unavailable, clusterName string) *Provider {
	return &Provider{
		servers:     servers,
		unavailable: unavailable,
		clusterName: clusterName,
		maxAttempts: DefaultMaxCreateAttempts,
		cache:       newListCache(servers, DefaultListTTL, nil),
	}
}

// errNoClusterName guards every path whose selector would otherwise match the
// entire Hetzner project, control plane included.
var errNoClusterName = errors.New("cluster name is empty, refusing to act on unowned servers")

// errNotManaged reports a server this cluster's provider does not own.
//
// A distinct type rather than a string, so a caller can tell "I will not touch
// this" apart from "the API call failed". The first must never be retried and
// must be loud: something asked us to delete a machine that is not ours.
type errNotManaged struct {
	name string
	id   int64
}

func (e errNotManaged) Error() string {
	return fmt.Sprintf("server %q (id %d) does not carry this cluster's %s label; refusing to touch it",
		e.name, e.id, hcloudapi.LabelManagedBy)
}

// Create orders a server for nodeClaim, falling through to the next-cheapest
// candidate on a capacity failure.
//
// The price ordering is recomputed here from this provider's own catalog
// because core loses its own in transit: scheduling/nodeclaimtemplate.go does
// OrderByPrice and serialises the result into a NodeSelectorRequirement on
// node.kubernetes.io/instance-type, but Requirements is a Go map and
// NodeSelectorRequirements() iterates it unordered. A provider that walks the
// requirement's values, or the API's listing order, picks essentially at
// random. Core's requirement is used here only as a filter.
func (p *Provider) Create(
	ctx context.Context,
	nodeClass *v1alpha1.HCloudNodeClass,
	nodeClaim *karpv1.NodeClaim,
	instanceTypes []*cloudprovider.InstanceType,
	userData string,
) (*hcloudapi.Server, *cloudprovider.InstanceType, error) {
	candidates := p.orderedCandidates(nodeClaim, instanceTypes)
	if len(candidates) == 0 {
		return nil, nil, cloudprovider.NewInsufficientCapacityError(
			fmt.Errorf("no instance type in this NodeClaim's requirements is offered in any location the NodeClass allows"))
	}

	req, err := p.baseRequest(nodeClass, nodeClaim, userData)
	if err != nil {
		return nil, nil, err
	}

	attempts := min(len(candidates), p.maxAttempts)
	started := time.Now()
	var lastErr error
	for i := range attempts {
		c := candidates[i]
		req.ServerType = c.InstanceType.Name
		req.Location = c.Location

		srv, err := p.servers.Create(ctx, req)
		if err == nil {
			// Measured across the WHOLE Create, fall-through included: the
			// expensive case is the one that walked several out-of-stock
			// candidates.
			metrics.LaunchDuration.WithLabelValues(c.InstanceType.Name, c.Location, "succeeded").
				Observe(time.Since(started).Seconds())
			// So the next List reflects a server we just made: otherwise core's
			// garbage collector reads a listing predating the create and reaps
			// the NodeClaim behind a live node.
			p.cache.invalidate()
			return srv, c.InstanceType, nil
		}
		lastErr = err

		class := hcloudapi.Classify(err)
		code, _ := hcloudapi.Code(err)
		metrics.LaunchFailures.WithLabelValues(c.InstanceType.Name, c.Location, class.String()).Inc()
		// An unrecognised code is retried as transient, safely and silently.
		// Counting it is how a new terminal code becomes visible instead of
		// being retried forever.
		if code != "" && !hcloudapi.IsKnownCode(err) {
			metrics.UnknownErrorCodes.WithLabelValues(code).Inc()
		}

		switch class {
		case hcloudapi.ClassCapacity:
			// Fires on every capacity-class code, not only
			// resource_unavailable: placement_error and
			// no_space_left_in_location are equally "not here, not now".
			p.unavailable.Mark(c.InstanceType.Name, c.Location, code)
			metrics.OfferingUnavailable.WithLabelValues(c.InstanceType.Name, c.Location, code).Set(1)
			log.FromContext(ctx).V(1).Info("capacity unavailable, falling through to the next candidate",
				"serverType", c.InstanceType.Name, "location", c.Location, "code", code)
			continue

		case hcloudapi.ClassConfig:
			if code == hcloudapi.CodeUniqueness {
				// A previous attempt's HTTP response was lost, but the server
				// was created. Adopt it rather than failing: the alternative
				// leaks a running, billing server that nothing owns.
				adopted, aErr := p.adopt(ctx, req.Name, nodeClaim)
				if aErr != nil {
					// Loud, always: a refusal means something else already holds
					// this name, and swallowing it would report a generic
					// create failure instead.
					log.FromContext(ctx).Error(aErr, "refusing to adopt an existing server", "server", req.Name)
				} else if adopted != nil {
					// The type ACTUALLY adopted, not the candidate this attempt
					// was trying: the ordering can differ between the lost
					// create and this retry, and core never re-reads capacity
					// from the Node, so a wrong type binpacks every future pod
					// against a machine that does not exist.
					adoptedType := findInstanceType(instanceTypes, adopted.ServerType)
					if adoptedType == nil {
						log.FromContext(ctx).Info("adopted a server whose type is not in the current catalog; "+
							"publishing no capacity rather than a wrong one",
							"server", adopted.Name, "serverType", adopted.ServerType)
					}
					log.FromContext(ctx).Info("adopted a server from a lost create response",
						"server", adopted.Name, "id", adopted.ID, "serverType", adopted.ServerType)
					p.cache.invalidate()
					metrics.LaunchDuration.WithLabelValues(adopted.ServerType, adopted.Location, "adopted").
						Observe(time.Since(started).Seconds())
					return adopted, adoptedType, nil
				}
			}
			// Anything else here will not succeed on another server type,
			// because it is a statement about the request, not about capacity.
			metrics.LaunchDuration.WithLabelValues(c.InstanceType.Name, c.Location, "failed").Observe(time.Since(started).Seconds())
			return nil, nil, cloudprovider.NewCreateError(err, "CreateFailed", err.Error())

		case hcloudapi.ClassQuota:
			// A different server type does not help: the project is at a limit.
			// Surfaced as insufficient capacity so core stops trying, rather
			// than as a create error that would fail the NodeClaim.
			metrics.LaunchDuration.WithLabelValues(c.InstanceType.Name, c.Location, "quota").Observe(time.Since(started).Seconds())
			return nil, nil, cloudprovider.NewInsufficientCapacityError(err)

		case hcloudapi.ClassFatal:
			log.FromContext(ctx).Error(err, "the Hetzner credential was rejected while creating a server; "+
				"every retry burns rate limit shared with the CCM and the CSI driver")
			metrics.LaunchDuration.WithLabelValues(c.InstanceType.Name, c.Location, "credential").Observe(time.Since(started).Seconds())
			return nil, nil, cloudprovider.NewCreateError(err, "CredentialRejected", err.Error())

		default:
			// Transient. Return rather than burn the remaining candidates on
			// what is probably a network problem; core retries with backoff.
			metrics.LaunchDuration.WithLabelValues(c.InstanceType.Name, c.Location, "transient").Observe(time.Since(started).Seconds())
			return nil, nil, fmt.Errorf("creating server, %w", err)
		}
	}

	// Recorded on the exhausted path too: a Create that walked every candidate
	// is the slowest thing this provider does, and observing only on success
	// makes the result label a constant and hides it. Labelled with the LAST
	// candidate tried, so the series says where the search gave up.
	last := candidates[attempts-1]
	metrics.LaunchDuration.WithLabelValues(last.InstanceType.Name, last.Location, "exhausted").
		Observe(time.Since(started).Seconds())
	return nil, nil, cloudprovider.NewInsufficientCapacityError(
		fmt.Errorf("no capacity after %d attempts across %d candidates, last error: %w", attempts, len(candidates), lastErr))
}

// HasCandidates reports whether any (type, location) pair could satisfy this
// NodeClaim, without touching the Hetzner API.
//
// Exists so the caller can avoid minting a join token, a live cluster-join
// credential, for a NodeClaim that has nowhere to go. It reuses Create's
// function so both agree on what a valid candidate is.
func (p *Provider) HasCandidates(nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) bool {
	return len(p.orderedCandidates(nodeClaim, instanceTypes)) > 0
}

// findInstanceType returns the catalog entry for a server type, or nil.
//
// nil is a meaningful answer, not a failure: an adopted server may be a type
// the catalog no longer offers, and publishing no capacity beats publishing
// another type's.
func findInstanceType(instanceTypes []*cloudprovider.InstanceType, name string) *cloudprovider.InstanceType {
	for _, it := range instanceTypes {
		if it.Name == name {
			return it
		}
	}
	return nil
}

// adopt recovers the server from a create whose response never arrived.
//
// Matched on BOTH our ownership label and the NodeClaim label, never the name
// alone: a collision with something we do not own must fail loudly rather than
// hand another system's server to karpenter, which would then delete it.
func (p *Provider) adopt(ctx context.Context, name string, nodeClaim *karpv1.NodeClaim) (*hcloudapi.Server, error) {
	srv, err := p.servers.GetByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, fmt.Errorf("server %q reported as duplicate but not found", name)
	}
	if !srv.IsManagedBy(p.clusterName) || srv.Labels[hcloudapi.LabelNodeClaim] != nodeClaim.Name {
		return nil, fmt.Errorf("server %q exists but is not ours to adopt", name)
	}
	return srv, nil
}

// orderedCandidates expands instance types into (type, location) pairs that
// satisfy the NodeClaim, cheapest first.
func (p *Provider) orderedCandidates(nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) []Candidate {
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)

	var out []Candidate
	for _, it := range instanceTypes {
		// Intersects, NOT Requirements.Compatible: Compatible iterates the
		// ARGUMENT's keys and rejects any non-well-known key the RECEIVER does
		// not declare. Core stamps karpenter.itsh.dev/hcloudnodeclass into every
		// NodeClaim's requirements and no instance type declares that key, so
		// Compatible would reject EVERY type for EVERY real NodeClaim: create
		// reports insufficient capacity, core deletes the NodeClaim, and the
		// cluster churns with every pod pending and the metrics blaming Hetzner.
		// This mirrors core's own filter in provisioning/scheduling/nodeclaim.go,
		// Intersects at the type level and IsCompatible at the offering level.
		if it.Requirements.Intersects(reqs) != nil {
			continue
		}
		// Resources are checked once per type rather than per offering: an
		// offering differs only in where it is, never in what it is.
		if !resourcesFit(nodeClaim, it) {
			continue
		}
		for _, o := range it.Offerings {
			if !o.Available {
				continue
			}
			if !reqs.IsCompatible(o.Requirements, scheduling.AllowUndefinedWellKnownLabels) {
				continue
			}
			// Read as a single definite value, never Any(): Requirements.Get on
			// a missing key returns an Exists requirement whose Any() is a
			// RANDOM number, so an offering without a region would silently
			// order a server in a location named "8198044085188639281".
			region := o.Requirements.Get(corev1RegionLabel)
			if region.Len() != 1 {
				continue
			}
			out = append(out, Candidate{InstanceType: it, Location: region.Values()[0], Price: o.Price})
		}
	}

	// Cheapest first, then deterministic tiebreaks. Without the tiebreaks two
	// equally priced candidates swap order between passes, so a replacement
	// picks a different type each time and the fleet never settles.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Price != out[j].Price {
			return out[i].Price < out[j].Price
		}
		if out[i].InstanceType.Name != out[j].InstanceType.Name {
			return out[i].InstanceType.Name < out[j].InstanceType.Name
		}
		return out[i].Location < out[j].Location
	})
	return out
}

// resourcesFit reports whether the instance type can hold what the NodeClaim
// asked for, after the type's own overhead is taken out.
func resourcesFit(nodeClaim *karpv1.NodeClaim, it *cloudprovider.InstanceType) bool {
	allocatable := it.Allocatable()
	for name, requested := range nodeClaim.Spec.Resources.Requests {
		available, ok := allocatable[name]
		if !ok || available.Cmp(requested) < 0 {
			return false
		}
	}
	return true
}
