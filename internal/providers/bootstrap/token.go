package bootstrap

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// nodeClaimGV is karpenter core's group/version. Built rather than referenced:
// core does not export it, and constructing it avoids depending on the scheme
// having been registered first.
var nodeClaimGV = schema.GroupVersion{Group: apis.Group, Version: "v1"}

const (
	// TokenNamespace is where kubeadm bootstrap tokens must live.
	TokenNamespace = "kube-system"

	// DefaultTokenTTL bounds how long a minted join token stays valid.
	//
	// The floor is Karpenter core's node registration timeout, a hardcoded 15
	// minutes: a token expiring before core gives up produces a silent
	// replacement loop with no useful error anywhere. 30 minutes is twice that
	// and still bounds real exposure, since a node's user_data carries the live
	// token and is readable on the node and through the Hetzner console.
	DefaultTokenTTL = 30 * time.Minute

	tokenIDLength     = 6
	tokenSecretLength = 16

	// tokenAlphabet is what kubeadm accepts: [a-z0-9].
	tokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// TokenMinter creates short-lived kubeadm bootstrap tokens, one per NodeClaim.
//
// One expiring token per node keeps a join credential from becoming a standing
// one: a shared or non-expiring token would be a permanent cluster-join
// credential readable from every machine's userdata.
//
// Cleanup needs no controller here. An ownerReference on the NodeClaim (a
// namespaced object may reference a cluster-scoped owner, only the reverse is
// forbidden) collects the Secret when the NodeClaim goes away, including on
// failed registration, revoking EARLIER than the token's own expiry;
// kube-controller-manager's tokencleaner covers expiry independently.
//
// Neither path lists secrets, so the RBAC here is create and delete with NO
// get, list or watch. That absence blocks the standard escalation of minting a
// service-account-token secret for a privileged service account and reading it.
type TokenMinter struct {
	client client.Client
	ttl    time.Duration
	now    func() time.Time
}

// NewTokenMinter returns a minter using the default TTL.
func NewTokenMinter(c client.Client) *TokenMinter {
	return &TokenMinter{client: c, ttl: DefaultTokenTTL, now: time.Now}
}

// NewTokenMinterWithOptions returns a minter with an explicit TTL and clock.
func NewTokenMinterWithOptions(c client.Client, ttl time.Duration, now func() time.Time) *TokenMinter {
	if now == nil {
		now = time.Now
	}
	return &TokenMinter{client: c, ttl: ttl, now: now}
}

// Mint creates a bootstrap token owned by nodeClaim and returns "id.secret".
func (m *TokenMinter) Mint(ctx context.Context, nodeClaim *karpv1.NodeClaim, clusterName string) (string, error) {
	if nodeClaim == nil || nodeClaim.Name == "" {
		return "", fmt.Errorf("a named NodeClaim is required to own the token")
	}
	if nodeClaim.UID == "" {
		// Without a UID the ownerReference is not resolvable, so the Secret
		// would outlive the NodeClaim and the primary revocation path would be
		// silently absent.
		return "", fmt.Errorf("NodeClaim %s has no UID; the token would not be garbage collected", nodeClaim.Name)
	}

	id, err := randomString(tokenIDLength)
	if err != nil {
		return "", err
	}
	secret, err := randomString(tokenSecretLength)
	if err != nil {
		return "", err
	}

	expiry := m.now().Add(m.ttl).UTC().Format(time.RFC3339)

	obj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: TokenNamespace,
			Name:      "bootstrap-token-" + id,
			Labels: map[string]string{
				"karpenter.sh/managed-by":      clusterName,
				"karpenter.itsh.dev/nodeclaim": nodeClaim.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: nodeClaimGV.String(),
				Kind:       "NodeClaim",
				Name:       nodeClaim.Name,
				UID:        nodeClaim.UID,
			}},
		},
		Type: corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			"token-id":                       id,
			"token-secret":                   secret,
			"expiration":                     expiry,
			"description":                    "karpenter " + nodeClaim.Name,
			"usage-bootstrap-authentication": "true",
			// Required, and not redundant with the CA pin. It makes
			// kube-controller-manager's bootstrapsigner publish a JWS signature
			// into cluster-info, without which kubeadm refuses to join. The CA
			// hash proves which cluster is being joined; the JWS proves the
			// anonymously fetched cluster-info came from someone who knows this
			// token.
			"usage-bootstrap-signing": "true",
			// Kubernetes enforces the system:bootstrappers: prefix here, which
			// is what bounds this from being a path to system:masters.
			"auth-extra-groups": "system:bootstrappers:kubeadm:default-node-token",
		},
	}

	if err := m.client.Create(ctx, obj); err != nil {
		return "", fmt.Errorf("creating bootstrap token: %w", err)
	}
	return id + "." + secret, nil
}

// Revoke deletes the token for a NodeClaim on a best-effort basis.
//
// Not required for correctness, since the ownerReference and tokencleaner cover
// it, but it makes revocation immediate on an orderly delete.
func (m *TokenMinter) Revoke(ctx context.Context, tokenID string) error {
	obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: TokenNamespace,
		Name:      "bootstrap-token-" + tokenID,
	}}
	if err := m.client.Delete(ctx, obj); err != nil {
		return fmt.Errorf("deleting bootstrap token: %w", err)
	}
	return nil
}

// TokenID returns the id half of an "id.secret" token.
func TokenID(token string) string {
	if len(token) <= tokenIDLength || token[tokenIDLength] != '.' {
		return ""
	}
	return token[:tokenIDLength]
}

// randomString draws from crypto/rand. Deliberately not math/rand: this is a
// credential that admits a node to the cluster.
func randomString(n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(tokenAlphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generating token: %w", err)
		}
		out[i] = tokenAlphabet[idx.Int64()]
	}
	return string(out), nil
}
