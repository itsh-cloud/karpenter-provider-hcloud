package bootstrap

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	// ClusterInfoNamespace and ClusterInfoName locate the ConfigMap kubeadm
	// publishes for joining nodes. It is world-readable by design.
	ClusterInfoNamespace = "kube-public"
	ClusterInfoName      = "cluster-info"
)

// ConfigError marks a cluster-info failure that retrying will not fix: the
// ConfigMap is absent, the read is forbidden, or the embedded kubeconfig is
// unusable.
//
// The distinction is load-bearing for the caller. A configuration failure has
// to be published on the NodeClass, because nothing else names it. A transport
// failure must NOT be, because reporting a blip as a configuration error takes
// every NodeClass to Ready=False, which makes karpenter core stop provisioning
// and delete in-flight NodeClaims over a rolling apiserver restart. The default
// is therefore transient: anything not recognised here is retried rather than
// published, which costs a slow diagnosis at worst.
type ConfigError struct{ err error }

// NewConfigError marks err as a permanent configuration failure.
func NewConfigError(err error) *ConfigError { return &ConfigError{err: err} }

func (e *ConfigError) Error() string { return e.err.Error() }
func (e *ConfigError) Unwrap() error { return e.err }

// asConfigError wraps err when it is a permanent configuration failure, and
// returns it untouched when it is worth retrying.
func asConfigError(err error) error {
	switch {
	case apierrors.IsNotFound(err),
		apierrors.IsForbidden(err),
		apierrors.IsUnauthorized(err),
		apierrors.IsBadRequest(err):
		return &ConfigError{err: err}
	default:
		// Timeouts, 429s, 500s, a closed connection during a control-plane
		// upgrade, and a cancelled context all land here.
		return err
	}
}

// Discovery resolves the join endpoint and CA pins from the cluster itself,
// at runtime.
//
// Reading them live rather than templating them in means there is no second
// copy to drift: a CA rotation or an endpoint change is picked up on the next
// refresh, and a stale value cannot be baked into a manifest where it fails
// only at the point a node tries to join.
type Discovery struct {
	// A Reader rather than a Client, deliberately: the only sane RBAC grant
	// for this is `get` on the single cluster-info ConfigMap in kube-public,
	// and a cached client would need list+watch on ConfigMaps to build its
	// informer, so it would wedge the whole manager waiting for a cache that
	// can never sync. Callers pass an uncached reader.
	client client.Reader

	mu       sync.RWMutex
	endpoint string
	hashes   []string
}

// NewDiscoveryFromManager returns a Discovery backed by the manager's direct
// API reader. This is the constructor production code must use.
//
// It exists because the mistake it prevents is invisible. NewDiscovery takes a
// client.Reader, which manager.GetClient() also satisfies, so passing the
// cached client compiles cleanly. At runtime the delegating client lazily
// starts a cluster-wide ConfigMap informer on first Get; the only sane RBAC for
// this is `get` on one named ConfigMap, so the informer's list is denied, the
// cache never syncs, and manager.Start blocks in WaitForCacheSync. The symptom
// is a readyz failure that never mentions ConfigMaps.
func NewDiscoveryFromManager(m manager.Manager) *Discovery {
	return &Discovery{client: m.GetAPIReader()}
}

// NewDiscovery returns a Discovery backed by the given reader.
//
// The reader must read straight from the apiserver. Prefer
// NewDiscoveryFromManager; this constructor is for tests, which pass a fake.
func NewDiscovery(c client.Reader) *Discovery { return &Discovery{client: c} }

// Refresh reads kube-public/cluster-info and caches what it finds.
//
// These values change only on CA rotation or an endpoint move, so this is
// cheap to hold and refresh on a watch rather than per NodeClaim.
func (d *Discovery) Refresh(ctx context.Context) error {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: ClusterInfoNamespace, Name: ClusterInfoName}
	if err := d.client.Get(ctx, key, &cm); err != nil {
		return asConfigError(fmt.Errorf("reading %s/%s: %w", ClusterInfoNamespace, ClusterInfoName, err))
	}

	raw, ok := cm.Data["kubeconfig"]
	if !ok {
		return &ConfigError{err: fmt.Errorf("%s/%s has no kubeconfig key", ClusterInfoNamespace, ClusterInfoName)}
	}

	// The ConfigMap was read, so anything wrong with it now is its content.
	endpoint, hashes, err := parseClusterInfo([]byte(raw))
	if err != nil {
		return &ConfigError{err: err}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.endpoint, d.hashes = endpoint, hashes
	return nil
}

// Get returns the discovered endpoint and CA pins.
func (d *Discovery) Get() (endpoint string, hashes []string, ok bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.endpoint == "" || len(d.hashes) == 0 {
		return "", nil, false
	}
	return d.endpoint, append([]string(nil), d.hashes...), true
}

// Resolve returns the endpoint and pins to use, preferring explicit overrides
// from the NodeClass over what was discovered.
//
// An override is occasionally necessary: cluster-info advertises whatever the
// control plane was initialised with, which may be a public address that nodes
// should not join over.
func (d *Discovery) Resolve(endpointOverride string, hashOverride []string) (string, []string, error) {
	endpoint, hashes, ok := d.Get()
	if endpointOverride != "" {
		endpoint = endpointOverride
	}
	if len(hashOverride) > 0 {
		hashes = hashOverride
	}
	if endpoint == "" || len(hashes) == 0 {
		if !ok {
			return "", nil, fmt.Errorf("cluster-info has not been read yet and no override was given")
		}
		return "", nil, fmt.Errorf("no API server endpoint or CA pin available")
	}
	return endpoint, hashes, nil
}

// parseClusterInfo extracts the endpoint as host:port and the CA public-key
// pins from an embedded kubeconfig.
func parseClusterInfo(raw []byte) (string, []string, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return "", nil, fmt.Errorf("parsing cluster-info kubeconfig: %w", err)
	}

	// kubeadm writes a single unnamed cluster; take whichever is present
	// rather than assuming the key.
	var server string
	var caPEM []byte
	for _, c := range cfg.Clusters {
		if c == nil {
			continue
		}
		server, caPEM = c.Server, c.CertificateAuthorityData
		break
	}
	if server == "" {
		return "", nil, fmt.Errorf("cluster-info kubeconfig has no server")
	}
	if len(caPEM) == 0 {
		return "", nil, fmt.Errorf("cluster-info kubeconfig has no certificate-authority-data")
	}

	endpoint, err := hostPort(server)
	if err != nil {
		return "", nil, err
	}

	hashes, err := caCertHashes(caPEM)
	if err != nil {
		return "", nil, err
	}
	return endpoint, hashes, nil
}

// hostPort turns https://10.1.0.2:6443 into 10.1.0.2:6443, which is the shape
// kubeadm's discovery.bootstrapToken.apiServerEndpoint expects.
func hostPort(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("parsing server URL %q: %w", server, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server URL %q has no host", server)
	}
	if u.Port() == "" {
		return u.Host + ":6443", nil
	}
	return u.Host, nil
}

// caCertHashes computes kubeadm's --discovery-token-ca-cert-hash values.
//
// The pin is over the SubjectPublicKeyInfo, NOT over the certificate, so it
// survives the CA certificate being reissued for the same key. Computing it
// with x509.MarshalPKIXPublicKey also keeps it algorithm-agnostic: the common
// shell equivalent pipes through `openssl rsa`, which silently fails on an
// ECDSA CA.
func caCertHashes(caPEM []byte) ([]string, error) {
	var hashes []string
	rest := caPEM

	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing CA certificate: %w", err)
		}
		spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("marshalling CA public key: %w", err)
		}
		sum := sha256.Sum256(spki)
		hashes = append(hashes, "sha256:"+hex.EncodeToString(sum[:]))
	}

	if len(hashes) == 0 {
		return nil, fmt.Errorf("no CERTIFICATE block in certificate-authority-data")
	}
	return hashes, nil
}

// ValidHash reports whether s looks like a kubeadm CA pin.
func ValidHash(s string) bool {
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}
