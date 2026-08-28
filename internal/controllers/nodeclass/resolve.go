package nodeclass

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

const (
	// resolvedRequeue is how often a successfully resolved NodeClass is
	// re-checked. What it resolves moves on its own: Hetzner rebuilds named
	// images periodically, and a placement group's membership changes with
	// every node created or removed.
	resolvedRequeue = 5 * time.Minute

	// misconfiguredRequeue is how often a NodeClass whose configuration does not
	// resolve is re-checked. Faster than the resolved interval, because the
	// expected next event is an operator creating the missing resource.
	misconfiguredRequeue = time.Minute
)

// Reason suffixes appended to a per-resource prefix, e.g. "Image" + "NotFound".
//
// Reasons must match metav1.Condition's `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`,
// so an error string used as a reason is rejected by the apiserver at the patch
// rather than where the mistake was made. Error text belongs in the message.
const (
	reasonNotFound             = "NotFound"
	reasonInvalidConfiguration = "InvalidConfiguration"
	reasonCredentialRejected   = "CredentialRejected"
)

// configFailure reports whether err is a permanent configuration problem that
// belongs on the NodeClass as a False condition, and the reason suffix to use.
//
// A missing resource or a rejected token will not fix itself by being retried,
// and hiding it behind a generic retry leaves nothing naming the wrong selector.
// A transport failure is the opposite: reporting it as configuration would take
// a healthy NodeClass to Ready=False and make core delete in-flight NodeClaims.
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
		// is classified transient on purpose. Unrecognised codes are logged
		// because that path retries forever: a new Hetzner code that ought to
		// be terminal is otherwise indistinguishable from a slow network.
		if code, ok := hcloudapi.Code(err); ok && !hcloudapi.IsKnownCode(err) {
			log.FromContext(ctx).Error(err, "unrecognised Hetzner error code, retrying as transient", "code", code)
		}
		return "", false
	}
}

// noteUnreachable records a transient failure on a condition that has NEVER
// resolved, and does nothing otherwise.
//
// An already-True condition must survive a Hetzner outage untouched, since
// downgrading it propagates to NodeClassReady and stops the NodePool over a
// blip. A condition still at its initialisation value has nothing to protect,
// and blocked egress, DNS failure or a new error code all classify transient and
// would otherwise retry forever with no diagnosis.
func noteUnreachable(conds status.ConditionSet, conditionType, kind string, err error) {
	if !conds.Get(conditionType).IsUnknown() {
		return
	}
	conds.SetUnknownWithReason(conditionType, kind+"Unreachable",
		fmt.Sprintf("the Hetzner API could not be reached to resolve the %s, retrying: %s", kind, err))
}

// failureMessage renders the condition message for a resolve failure.
//
// A rejected credential gets a fixed message that names the credential: the
// generic form produces up to five simultaneous conditions each blaming a
// different selector, all correct, with nothing pointing at the token.
func failureMessage(what, reason string, err error) string {
	if reason == reasonCredentialRejected {
		return fmt.Sprintf("the Hetzner API rejected this controller's credential while resolving %s: %s. "+
			"The token is wrong, revoked, or read-only; the selector is not at fault", what, err)
	}
	return fmt.Sprintf("resolving %s: %s", what, err)
}
