package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"
)

// makeCA returns a self-signed CA certificate in PEM form plus the SPKI hash
// computed independently of the code under test, so the assertion is not
// simply the implementation restated.
func makeCA(t *testing.T, ecdsaKey bool) (pemBytes []byte, wantHash string) {
	t.Helper()

	var pub any
	var signer any
	if ecdsaKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		pub, signer = &k.PublicKey, k
	} else {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		pub, signer = &k.PublicKey, k
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, signer)
	if err != nil {
		t.Fatal(err)
	}

	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"sha256:" + hex.EncodeToString(sum[:])
}

func kubeconfig(server string, caPEM []byte) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: %s
    server: %s
  name: ""
contexts: null
current-context: ""
preferences: {}
users: null
`, base64.StdEncoding.EncodeToString(caPEM), server)
}

// TestParseClusterInfoRSA covers the ordinary case.
func TestParseClusterInfoRSA(t *testing.T) {
	caPEM, wantHash := makeCA(t, false)

	endpoint, hashes, err := parseClusterInfo([]byte(kubeconfig("https://10.1.0.2:6443", caPEM)))
	if err != nil {
		t.Fatalf("parseClusterInfo: %v", err)
	}
	if endpoint != "10.1.0.2:6443" {
		t.Errorf("endpoint = %q, want 10.1.0.2:6443", endpoint)
	}
	if len(hashes) != 1 || hashes[0] != wantHash {
		t.Errorf("hashes = %v, want [%s]", hashes, wantHash)
	}
}

// TestParseClusterInfoECDSA is the case the shell equivalent gets wrong.
//
// The widely-copied recipe pipes through `openssl rsa -pubin`, which fails on
// an ECDSA CA and, in a pipeline, tends to yield a hash of nothing rather than
// an error. Deriving the pin with x509.MarshalPKIXPublicKey is
// algorithm-agnostic.
func TestParseClusterInfoECDSA(t *testing.T) {
	caPEM, wantHash := makeCA(t, true)

	_, hashes, err := parseClusterInfo([]byte(kubeconfig("https://10.1.0.2:6443", caPEM)))
	if err != nil {
		t.Fatalf("parseClusterInfo with an ECDSA CA: %v", err)
	}
	if len(hashes) != 1 || hashes[0] != wantHash {
		t.Errorf("hashes = %v, want [%s]", hashes, wantHash)
	}
}

// TestCACertHashesHandlesBundles: a rotation leaves two certificates in the
// bundle, and pinning only the first would refuse the new one.
func TestCACertHashesHandlesBundles(t *testing.T) {
	a, hashA := makeCA(t, false)
	b, hashB := makeCA(t, true)

	hashes, err := caCertHashes(append(a, b...))
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 2 || hashes[0] != hashA || hashes[1] != hashB {
		t.Errorf("hashes = %v, want [%s %s]", hashes, hashA, hashB)
	}
}

func TestHostPort(t *testing.T) {
	tests := []struct {
		server string
		want   string
		errors bool
	}{
		{"https://10.1.0.2:6443", "10.1.0.2:6443", false},
		{"https://api.example.com:6443", "api.example.com:6443", false},
		// kubeadm's discovery field wants host:port, so a bare URL needs the
		// default filled in rather than being passed through.
		{"https://10.1.0.2", "10.1.0.2:6443", false},
		{"https://[2001:db8::1]:6443", "[2001:db8::1]:6443", false},
		{"", "", true},
		{"://nonsense", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.server, func(t *testing.T) {
			got, err := hostPort(tt.server)
			if tt.errors {
				if err == nil {
					t.Fatalf("expected an error for %q", tt.server)
				}
				return
			}
			if err != nil {
				t.Fatalf("hostPort(%q): %v", tt.server, err)
			}
			if got != tt.want {
				t.Errorf("hostPort(%q) = %q, want %q", tt.server, got, tt.want)
			}
		})
	}
}

func TestParseClusterInfoRejectsIncomplete(t *testing.T) {
	caPEM, _ := makeCA(t, false)

	tests := []struct {
		name string
		raw  string
	}{
		{"no server", kubeconfig("", caPEM)},
		{"no CA data", kubeconfig("https://10.1.0.2:6443", nil)},
		{"not a kubeconfig", "just some text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseClusterInfo([]byte(tt.raw)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// TestCACertHashesRejectsNonCertificatePEM: a bundle with no CERTIFICATE block
// must error rather than yield an empty pin list, which would render a join
// configuration with no CA pinning at all.
func TestCACertHashesRejectsNonCertificatePEM(t *testing.T) {
	notACert := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("nope")})
	if _, err := caCertHashes(notACert); err == nil {
		t.Error("expected an error: an empty pin list would mean joining with no CA pin")
	}
}

func TestValidHash(t *testing.T) {
	good := "sha256:" + hex.EncodeToString(make([]byte, 32))
	tests := []struct {
		in   string
		want bool
	}{
		{good, true},
		{"sha256:abc", false},
		{hex.EncodeToString(make([]byte, 32)), false},
		{"sha512:" + hex.EncodeToString(make([]byte, 32)), false},
		{"sha256:" + repeat(64, 'z'), false},
		{"", false},
	}
	for _, tt := range tests {
		if got := ValidHash(tt.in); got != tt.want {
			t.Errorf("ValidHash(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func repeat(n int, c byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
