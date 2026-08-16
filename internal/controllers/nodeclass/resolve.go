package nodeclass

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

const (
	// resolvedRequeue is how often a successfully resolved NodeClass is
	// re-checked. The things it resolves move on their own: Hetzner rebuilds
	// named images periodically so a name maps to a new ID, and a placement
	// group's membership changes with every node created or removed.
	resolvedRequeue = 5 * time.Minute

	// misconfiguredRequeue is how often a NodeClass whose configuration does
	// not resolve is re-checked. Faster than the resolved interval, because the
	// expected next event is an operator creating the missing resource, and
	// leaving them staring at a stale condition for five minutes teaches them
	// the controller is broken.
	misconfiguredRequeue = time.Minute

	// catalogRequeue is how often a NodeClass waiting on the server type
	// catalog is re-checked. Short, because the wait is for one HTTP call this
	// process is already making, and every restart and leader-election failover
	// otherwise reads Ready=Unknown on every NodeClass for a full minute.
	catalogRequeue = 5 * time.Second
)

// Reason suffixes appended to a per-resource prefix, e.g. "Image" + "NotFound".
//
// Reasons are constrained by the CRD schema that controller-gen copies from
// metav1.Condition: `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`. An error string
// used as a reason is rejected by the apiserver, and the reconcile then fails
// at the patch rather than at the point the mistake was made, so error text
// only ever belongs in the message.
const (
	reasonNotFound             = "NotFound"
	reasonInvalidConfiguration = "InvalidConfiguration"
	reasonCredentialRejected   = "CredentialRejected"
)

// configFailure reports whether err is a permanent configuration problem that
// belongs on the NodeClass as a False condition, and the reason suffix to use.
//
// The distinction is the whole point of the split. A missing resource or a
// rejected token will not fix itself by being retried, and hiding it behind a
// generic retry means an operator sees nodes failing to appear with nothing
// anywhere naming the selector that is wrong. A transport failure is the exact
// opposite: reporting it as a configuration error would take a healthy
// NodeClass to Ready=False, which makes karpenter core delete in-flight
// NodeClaims, over a blip.
func configFailure(ctx context.Context, err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var notFound *hcloudapi.NotFoundError
	if errors.As(err, &notFound) {
		return reasonNotFound, true
	}
	switch hcloudapi.Classify(err) {
	case hcloudapi.ClassConfig:
		return reasonInvalidConfiguration, true
	case hcloudapi.ClassFatal:
		// A wrong or read-only token is not transient, and every retry burns
		// rate limit that is shared with the CCM and the CSI driver.
		return reasonCredentialRejected, true
	default:
		// Capacity and quota cannot arise from a GET, and an unrecognised code
		// is classified transient on purpose. Retry all of them.
		//
		// Logged when the code was not one we recognise, because that path
		// retries forever and the condition it leaves behind names neither
		// Hetzner nor the code. A new Hetzner error code that ought to be
		// terminal is otherwise indistinguishable from a slow network.
		if code, ok := hcloudapi.Code(err); ok && !hcloudapi.IsKnownCode(err) {
			log.FromContext(ctx).Error(err, "unrecognised Hetzner error code, retrying as transient", "code", code)
		}
		return "", false
	}
}

// failureMessage renders the condition message for a resolve failure.
//
// A rejected credential gets a fixed message that names the credential. Left to
// the generic form it produces up to five simultaneous conditions that each
// blame a different selector, every one of which is correct, with nothing
// anywhere pointing at the token.
func failureMessage(what, reason string, err error) string {
	if reason == reasonCredentialRejected {
		return fmt.Sprintf("the Hetzner API rejected this controller's credential while resolving %s: %s. "+
			"The token is wrong, revoked, or read-only; the selector is not at fault", what, err)
	}
	return fmt.Sprintf("resolving %s: %s", what, err)
}
