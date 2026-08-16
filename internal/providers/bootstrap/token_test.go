package bootstrap

import (
	"context"
	"regexp"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func testClaim() *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "autoscaled-general-nbg1-x7k2p", UID: types.UID("abc-123")},
	}
}

func TestMintCreatesShortLivedToken(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := NewTokenMinterWithOptions(c, 30*time.Minute, func() time.Time { return now })

	claim := testClaim()
	token, err := m.Mint(context.Background(), claim, "prod")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// kubeadm's format: 6 lowercase alnum, a dot, 16 lowercase alnum.
	if !regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`).MatchString(token) {
		t.Fatalf("token %q does not match kubeadm's required format", token)
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: TokenNamespace, Name: "bootstrap-token-" + TokenID(token)}
	if err := c.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("token secret not found: %v", err)
	}

	if secret.Type != corev1.SecretTypeBootstrapToken {
		t.Errorf("type = %q, want %q", secret.Type, corev1.SecretTypeBootstrapToken)
	}

	data := secret.StringData

	// The expiry is the entire point: the token this replaces had none at all,
	// so it stayed valid forever and was readable from every node's userdata.
	wantExpiry := now.Add(30 * time.Minute).UTC().Format(time.RFC3339)
	if data["expiration"] != wantExpiry {
		t.Errorf("expiration = %q, want %q", data["expiration"], wantExpiry)
	}
	if data["expiration"] == "" {
		t.Error("token has no expiration; it would never be garbage collected")
	}

	// Kubernetes enforces the system:bootstrappers: prefix here, which is what
	// keeps this from being a path to system:masters.
	if got := data["auth-extra-groups"]; got != "system:bootstrappers:kubeadm:default-node-token" {
		t.Errorf("auth-extra-groups = %q", got)
	}
	// Must be true. Verified the hard way against a real cluster: with this
	// false, kubeadm join fails at preflight with "could not find a JWS
	// signature in the cluster-info ConfigMap for token ID". The CA pin and
	// the JWS are not redundant, and no unit test catches this.
	if got := data["usage-bootstrap-signing"]; got != "true" {
		t.Errorf("usage-bootstrap-signing = %q, want true; kubeadm join fails at "+
			"preflight without the JWS signature bootstrapsigner writes for this token", got)
	}
	if got := data["usage-bootstrap-authentication"]; got != "true" {
		t.Errorf("usage-bootstrap-authentication = %q, want true", got)
	}
}

// TestOwnerReferenceEnablesGarbageCollection.
//
// This is the primary revocation path. A namespaced Secret may reference a
// cluster-scoped owner (only the reverse is forbidden), so the token dies with
// the NodeClaim, including when registration fails and core deletes it. That
// revokes EARLIER than the token's own expiry, which is the safe direction.
func TestOwnerReferenceEnablesGarbageCollection(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := NewTokenMinter(c)

	claim := testClaim()
	token, err := m.Mint(context.Background(), claim, "prod")
	if err != nil {
		t.Fatal(err)
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: TokenNamespace, Name: "bootstrap-token-" + TokenID(token)}
	if err := c.Get(context.Background(), key, &secret); err != nil {
		t.Fatal(err)
	}

	if len(secret.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %v, want exactly one", secret.OwnerReferences)
	}
	ref := secret.OwnerReferences[0]
	if ref.Kind != "NodeClaim" || ref.Name != claim.Name || ref.UID != claim.UID {
		t.Errorf("ownerReference = %+v, does not point at the NodeClaim", ref)
	}
	// BlockOwnerDeletion must stay unset: blocking would make the NodeClaim
	// undeletable until the Secret goes, which is backwards.
	if ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion {
		t.Error("BlockOwnerDeletion is set; the NodeClaim must not be held up by its token")
	}
}

// TestMintRefusesClaimWithoutUID: without a UID the ownerReference does not
// resolve, so the Secret would outlive the NodeClaim and the primary
// revocation path would be silently absent.
func TestMintRefusesClaimWithoutUID(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := NewTokenMinter(c)

	claim := testClaim()
	claim.UID = ""

	if _, err := m.Mint(context.Background(), claim, "prod"); err == nil {
		t.Error("expected an error: the token would never be garbage collected")
	}
}

// TestTokensAreUnique: a shared token is what this design exists to remove, so
// two NodeClaims must never receive the same one.
func TestTokensAreUnique(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := NewTokenMinter(c)

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		claim := testClaim()
		claim.UID = types.UID(regexp.MustCompile(`\W`).ReplaceAllString(time.Now().String()+string(rune(i)), ""))
		token, err := m.Mint(context.Background(), claim, "prod")
		if err != nil {
			t.Fatalf("Mint %d: %v", i, err)
		}
		if seen[token] {
			t.Fatalf("duplicate token issued: %q", token)
		}
		seen[token] = true
	}
}

func TestTokenID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"abcdef.0123456789abcdef", "abcdef"},
		{"tooshort", ""},
		{"", ""},
		{"abcdefg0123456789abcdef", ""}, // no dot in position 6
	}
	for _, tt := range tests {
		if got := TokenID(tt.in); got != tt.want {
			t.Errorf("TokenID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRevokeDeletesTheSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := NewTokenMinter(c)

	token, err := m.Mint(context.Background(), testClaim(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	id := TokenID(token)

	if err := m.Revoke(context.Background(), id); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: TokenNamespace, Name: "bootstrap-token-" + id}
	if err := c.Get(context.Background(), key, &secret); err == nil {
		t.Error("secret still present after Revoke")
	}
}

// TestTTLIsAboveRegistrationTimeout.
//
// Karpenter core's node registration timeout is a hardcoded 15 minutes. A
// token expiring inside that window produces a silent replacement loop: the
// node cannot join, core gives up, launches another, and the same thing
// happens, with no error naming the cause.
func TestTTLIsAboveRegistrationTimeout(t *testing.T) {
	const coreRegistrationTimeout = 15 * time.Minute
	if DefaultTokenTTL <= coreRegistrationTimeout {
		t.Errorf("DefaultTokenTTL %v is not above core's %v registration timeout; "+
			"tokens would expire mid-join and produce a silent replacement loop",
			DefaultTokenTTL, coreRegistrationTimeout)
	}
}
