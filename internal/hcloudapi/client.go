package hcloudapi

import (
	"fmt"
	"os"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// TokenEnvVar is where the Hetzner Cloud API token is read from.
//
// Environment rather than a file path or a flag: the chart projects it from a
// Secret with envFrom, so it never lands on disk and never appears in the pod
// spec, in an argv the whole node can read from /proc, or in this process's own
// logs.
const TokenEnvVar = "HCLOUD_TOKEN"

// tokenLength is the fixed width of a Hetzner Cloud API token. Checked so that
// a truncated or whitespace-padded secret is named at startup rather than
// producing an unauthorized error on the first scale-up, which reads as a
// permissions problem rather than a malformed value.
const tokenLength = 64

// pollInterval is how often an in-flight Action is polled.
//
// Server creation is asynchronous, so every create costs one POST plus a poll
// until the action settles. hcloud-go defaults to 500ms, which against a
// 3600/hour per-project limit shared with the CCM and the CSI driver spends
// roughly two requests per second per pending server. Three seconds is well
// inside the time a create takes anyway.
const pollInterval = 3 * time.Second

// NewClientFromEnv builds the Hetzner API client from the environment.
func NewClientFromEnv() (*hcloud.Client, error) {
	token := os.Getenv(TokenEnvVar)
	if token == "" {
		return nil, fmt.Errorf("%s is not set", TokenEnvVar)
	}
	// Reported by length only. The value is a credential and must not reach a
	// log line, an error string that ends up on a NodeClass condition, or a
	// Kubernetes event.
	if len(token) != tokenLength {
		return nil, fmt.Errorf("%s is %d characters, expected %d; check the secret for a truncated value or trailing whitespace",
			TokenEnvVar, len(token), tokenLength)
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
