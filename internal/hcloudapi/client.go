package hcloudapi

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// TokenEnvVar is where the Hetzner Cloud API token is read from. Environment
// rather than a file or a flag: the chart projects it from a Secret with
// envFrom, so it never lands on disk, in the pod spec, in an argv the whole
// node can read from /proc, or in the logs.
const TokenEnvVar = "HCLOUD_TOKEN"

// tokenLength is the fixed width of a Hetzner Cloud API token. Checked so that
// a truncated or whitespace-padded secret is named at startup rather than
// surfacing as an unauthorized error on the first scale-up, which reads as a
// permissions problem rather than a malformed value.
const tokenLength = 64

// pollInterval is how often an in-flight Action is polled. hcloud-go defaults
// to 500ms, which against a 3600/hour per-project limit shared with the CCM and
// the CSI driver spends roughly two requests per second per pending server.
// Three seconds is well inside the time a create takes anyway.
const pollInterval = 3 * time.Second

// NewClientFromEnv builds the Hetzner API client from the environment.
func NewClientFromEnv() (*hcloud.Client, error) {
	token := os.Getenv(TokenEnvVar)
	if token == "" {
		return nil, fmt.Errorf("%s is not set", TokenEnvVar)
	}
	// Reported by shape only, never by value: the token must not reach a log
	// line, an error string that ends up on a NodeClass condition, or an event.
	//
	// Both checks exist so a malformed secret is named HERE, at startup. Left
	// to hcloud-go, a token carrying a character illegal in an HTTP header
	// fails on every call with an error carrying no Hetzner error code, which
	// Classify treats as transient, so the provider retries forever with
	// nothing on any NodeClass explaining why.
	if len(token) != tokenLength {
		return nil, fmt.Errorf("%s is %d characters, expected %d; check the secret for a truncated value or trailing whitespace",
			TokenEnvVar, len(token), tokenLength)
	}
	if idx := strings.IndexFunc(token, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}); idx >= 0 {
		return nil, fmt.Errorf("%s contains a non-alphanumeric character at position %d; check the secret for a newline or a copied-in quote",
			TokenEnvVar, idx)
	}

	return hcloud.NewClient(
		hcloud.WithToken(token),
		hcloud.WithApplication("karpenter-provider-hcloud", Version),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(pollInterval)}),
	), nil
}

// Version is stamped at build time and sent as the client's user agent, so that
// a rate limit or an odd request pattern can be traced back to a release.
var Version = "dev"
