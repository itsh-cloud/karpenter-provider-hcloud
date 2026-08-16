package hcloudapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// DefaultCreateTimeout bounds how long a create waits for its action to settle.
//
// Server creation is asynchronous: the POST returns an Action, and a placement
// failure can surface either synchronously in the POST or minutes later in that
// action. Waiting forever would pin a NodeClaim on a create that is never going
// to succeed; ninety seconds is comfortably above a normal create and well
// under karpenter core's fifteen-minute registration timeout, leaving room to
// fall through to another server type and still register.
const DefaultCreateTimeout = 90 * time.Second

// Server is one Hetzner server in this provider's own terms.
type Server struct {
	ID         int64
	Name       string
	ServerType string
	// Location is the Hetzner location, e.g. nbg1.
	//
	// There is deliberately no Datacenter field. hcloud-go no longer exposes
	// one on a server, matching the retirement of the datacenter API, and this
	// provider orders by location and never by datacenter. The finer
	// nbg1-dc3 reaches Kubernetes as topology.kubernetes.io/zone, written by
	// hcloud-CCM from whichever datacenter the server actually landed in.
	Location string
	Status   string
	Labels   map[string]string
	ImageID  int64
	Created  time.Time

	PrivateIPv4 string
	PublicIPv4  string
}

// ProviderID is the value Kubernetes carries on Node.spec.providerID.
func (s *Server) ProviderID() string { return ProviderIDPrefix + strconv.FormatInt(s.ID, 10) }

// ProviderIDPrefix is the scheme hcloud-CCM uses, and must match it exactly:
// the CCM sets this on every Node, and karpenter matches NodeClaims to Nodes by
// string equality on it.
const ProviderIDPrefix = "hcloud://"

// ServerIDFromProviderID extracts the numeric id, or an error if the value is
// not one of ours.
func ServerIDFromProviderID(providerID string) (int64, error) {
	rest, ok := strings.CutPrefix(providerID, ProviderIDPrefix)
	if !ok {
		return 0, fmt.Errorf("providerID %q is not an hcloud provider id", providerID)
	}
	// ParseInt rather than a scan, so trailing rubbish is rejected instead of
	// silently truncated: the id this yields selects the server a delete acts
	// on.
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("providerID %q has no valid server id", providerID)
	}
	return id, nil
}

// CreateServerRequest is one fully resolved server order.
//
// Everything here is an id or a literal, never a selector: resolution happens
// in the NodeClass controllers and is published on its status, so a create can
// never race a selector that resolves differently between the decision and the
// order.
type CreateServerRequest struct {
	Name       string
	ServerType string
	Location   string
	ImageID    int64

	SSHKeyIDs        []int64
	NetworkIDs       []int64
	FirewallIDs      []int64
	PlacementGroupID *int64

	UserData string
	Labels   map[string]string

	PublicIPv4 bool
	PublicIPv6 bool
}

// Servers is the slice of the Hetzner server API this provider needs.
type Servers interface {
	Create(ctx context.Context, req CreateServerRequest) (*Server, error)
	Delete(ctx context.Context, id int64) error
	Get(ctx context.Context, id int64) (*Server, error)
	GetByName(ctx context.Context, name string) (*Server, error)
	// List returns every server matching a label selector. Server-side
	// filtering, so the response does not carry the whole project.
	List(ctx context.Context, labelSelector string) ([]*Server, error)
}

type serverClient struct {
	c       *hcloud.Client
	timeout time.Duration
}

// NewServers returns a Servers backed by the given hcloud client.
func NewServers(c *hcloud.Client) Servers {
	return NewServersWithTimeout(c, DefaultCreateTimeout)
}

// NewServersWithTimeout returns a Servers with an explicit create timeout.
func NewServersWithTimeout(c *hcloud.Client, timeout time.Duration) Servers {
	if timeout <= 0 {
		timeout = DefaultCreateTimeout
	}
	return &serverClient{c: c, timeout: timeout}
}

func (s *serverClient) Create(ctx context.Context, req CreateServerRequest) (*Server, error) {
	opts := hcloud.ServerCreateOpts{
		Name:       req.Name,
		ServerType: &hcloud.ServerType{Name: req.ServerType},
		Image:      &hcloud.Image{ID: req.ImageID},
		Location:   &hcloud.Location{Name: req.Location},
		UserData:   req.UserData,
		Labels:     req.Labels,
		// Explicitly true. The default is also true, but a server created
		// stopped never runs cloud-init, so it never joins, and the only
		// symptom is a NodeClaim that ages out after fifteen minutes.
		StartAfterCreate: hcloud.Ptr(true),
		// Volumes are attached by the CSI driver, never at create time. Leaving
		// automount on would mount whatever was passed into the filesystem
		// behind the CSI driver's back.
		Automount: hcloud.Ptr(false),
		PublicNet: &hcloud.ServerCreatePublicNet{
			EnableIPv4: req.PublicIPv4,
			EnableIPv6: req.PublicIPv6,
		},
	}
	for _, id := range req.SSHKeyIDs {
		opts.SSHKeys = append(opts.SSHKeys, &hcloud.SSHKey{ID: id})
	}
	for _, id := range req.NetworkIDs {
		opts.Networks = append(opts.Networks, &hcloud.Network{ID: id})
	}
	for _, id := range req.FirewallIDs {
		opts.Firewalls = append(opts.Firewalls, &hcloud.ServerCreateFirewall{Firewall: hcloud.Firewall{ID: id}})
	}
	if req.PlacementGroupID != nil {
		opts.PlacementGroup = &hcloud.PlacementGroup{ID: *req.PlacementGroupID}
	}

	result, _, err := s.c.Server.Create(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("creating server %q: %w", req.Name, err)
	}
	if result.Server == nil {
		return nil, fmt.Errorf("creating server %q: hetzner returned no server", req.Name)
	}

	// The action is waited on rather than assumed successful. A create that
	// returns 201 can still fail placement afterwards, and treating the 201 as
	// success produces a NodeClaim pointing at a server that will never exist.
	//
	// The leftover is deleted on failure: hcloud has already allocated the name
	// and the id, so leaving it behind both bills and blocks the retry with a
	// uniqueness_error on the same name.
	waitCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	actions := append([]*hcloud.Action{result.Action}, result.NextActions...)
	if err := s.c.Action.WaitFor(waitCtx, actions...); err != nil {
		s.deleteQuietly(context.WithoutCancel(ctx), result.Server.ID)
		return nil, fmt.Errorf("waiting for server %q to be created: %w", req.Name, err)
	}

	// Re-read rather than trusting the create response: the private IP is
	// assigned during the create action and is absent from the initial body.
	srv, err := s.Get(ctx, result.Server.ID)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, fmt.Errorf("server %q vanished immediately after creation", req.Name)
	}
	return srv, nil
}

// deleteQuietly removes a server on a best-effort basis, for the cleanup path
// where the caller is already returning an error.
//
// Deliberately takes a context detached from the failed operation: the common
// reason to be here is a create that timed out, and reusing that cancelled
// context would skip the cleanup exactly when it is needed.
func (s *serverClient) deleteQuietly(ctx context.Context, id int64) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _, _ = s.c.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id})
}

func (s *serverClient) Delete(ctx context.Context, id int64) error {
	_, _, err := s.c.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id})
	if err != nil {
		// Already gone is success. Karpenter retries Delete until it reports
		// not-found, so mapping this to an error would spin forever on a server
		// somebody removed by hand.
		if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
			return &NotFoundError{Kind: "server", Selector: fmt.Sprint(id)}
		}
		return fmt.Errorf("deleting server %d: %w", id, err)
	}
	return nil
}

func (s *serverClient) Get(ctx context.Context, id int64) (*Server, error) {
	srv, _, err := s.c.Server.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting server %d: %w", id, err)
	}
	if srv == nil {
		return nil, nil
	}
	return serverFromHcloud(srv), nil
}

func (s *serverClient) GetByName(ctx context.Context, name string) (*Server, error) {
	srv, _, err := s.c.Server.GetByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("getting server %q: %w", name, err)
	}
	if srv == nil {
		return nil, nil
	}
	return serverFromHcloud(srv), nil
}

func (s *serverClient) List(ctx context.Context, labelSelector string) ([]*Server, error) {
	srvs, err := s.c.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: labelSelector},
	})
	if err != nil {
		return nil, fmt.Errorf("listing servers %q: %w", labelSelector, err)
	}
	out := make([]*Server, 0, len(srvs))
	for _, srv := range srvs {
		if srv == nil {
			continue
		}
		out = append(out, serverFromHcloud(srv))
	}
	return out, nil
}

func serverFromHcloud(srv *hcloud.Server) *Server {
	out := &Server{
		ID:      srv.ID,
		Name:    srv.Name,
		Status:  string(srv.Status),
		Labels:  srv.Labels,
		Created: srv.Created,
	}
	if srv.ServerType != nil {
		out.ServerType = srv.ServerType.Name
	}
	if srv.Location != nil {
		out.Location = srv.Location.Name
	}
	if srv.Image != nil {
		out.ImageID = srv.Image.ID
	}
	if srv.PublicNet.IPv4.IP != nil {
		out.PublicIPv4 = srv.PublicNet.IPv4.IP.String()
	}
	// The first private network address. Nodes join over the private network,
	// so this is the address that matters, and a server with none is one that
	// cannot reach the API server.
	for _, n := range srv.PrivateNet {
		if n.IP != nil {
			out.PrivateIPv4 = n.IP.String()
			break
		}
	}
	return out
}

// IsNotFound reports whether err is this package's not-found.
func IsNotFound(err error) bool {
	var nf *NotFoundError
	return errors.As(err, &nf)
}
