package nodeclass

import (
	"context"
	"fmt"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// SSHKeys resolves spec.sshKeySelectors into status.sshKeys.
//
// Alone among the selectors, a missing key fails OPEN: nothing about joining
// needs SSH, so a deleted key only costs the ability to log in to nodes built
// afterwards, while failing closed would take every NodePool using the class out
// of service within one requeue. A rejected or read-only token still fails
// closed, because that says nothing about SSH keys, it means nothing on this
// NodeClass can be trusted to have resolved.
type SSHKeys struct {
	clk       clock.Clock
	recorder  events.Recorder
	resources hcloudapi.Resources
}

// NewSSHKeys returns the SSH key sub-reconciler.
func NewSSHKeys(clk clock.Clock, recorder events.Recorder, resources hcloudapi.Resources) *SSHKeys {
	return &SSHKeys{clk: clk, recorder: recorder, resources: resources}
}

func (s *SSHKeys) Reconcile(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	if len(nodeClass.Spec.SSHKeySelectors) == 0 {
		// No keys is a supported configuration, not an incomplete one, so True
		// rather than Unknown.
		nodeClass.Status.SSHKeys = nil
		nodeClass.StatusConditions(status.WithClock(s.clk)).SetTrue(v1alpha1.ConditionTypeSSHKeysReady)
		return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
	}

	resolved := make([]v1alpha1.SSHKeyStatus, 0, len(nodeClass.Spec.SSHKeySelectors))
	var missing []string
	var reason string
	var cause error
	// Accumulated across the whole loop, not read off the last failure: a single
	// overwritten variable makes fail-open versus fail-closed depend on
	// ITERATION ORDER, and in the open direction it publishes a partial key set
	// as success while blaming a key the API merely refused to answer for.
	failClosed := false

	for _, sel := range nodeClass.Spec.SSHKeySelectors {
		key, err := s.resources.SSHKey(ctx, sel.Name, sel.ID)
		if err != nil {
			r, ok := configFailure(ctx, err)
			if !ok {
				noteUnreachable(nodeClass.StatusConditions(status.WithClock(s.clk)), v1alpha1.ConditionTypeSSHKeysReady, "SSHKeys", err)
				return reconcile.Result{}, fmt.Errorf("resolving ssh key, %w", err)
			}
			if r != reasonNotFound {
				failClosed = true
			}
			// The reported reason is the most severe seen, not the most
			// recent, so it agrees with failClosed.
			if reason == "" || (r != reasonNotFound && reason == reasonNotFound) {
				reason, cause = r, err
			}
			missing = append(missing, describeSelector(sel.Name, sel.ID))
			continue
		}
		resolved = append(resolved, v1alpha1.SSHKeyStatus{ID: key.ID, Name: key.Name, Fingerprint: key.Fingerprint})
	}

	conds := nodeClass.StatusConditions(status.WithClock(s.clk))

	if failClosed {
		// A rejected credential or a malformed selector: not a statement about
		// SSH keys, so the fail-open argument above does not apply.
		nodeClass.Status.SSHKeys = nil
		conds.SetFalse(v1alpha1.ConditionTypeSSHKeysReady, "SSHKey"+reason,
			failureMessage("sshKeySelectors "+strings.Join(missing, ", "), reason, cause))
		return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
	}

	nodeClass.Status.SSHKeys = resolved
	if len(missing) > 0 {
		// True, and loud: hcloud does not return ssh_keys on a server GET, so a
		// node built with a reduced key set is not detectable from the API
		// afterwards and this condition is the only record of it. Published on
		// transition only, for the same reason as the location narrowing.
		message := fmt.Sprintf("continuing without sshKeySelectors %s, which no longer exist in the Hetzner project; "+
			"nodes built from now on will not accept those keys", strings.Join(missing, ", "))
		prev := conds.Get(v1alpha1.ConditionTypeSSHKeysReady)
		firstTime := prev == nil || prev.Reason != "SSHKeysNarrowed" || prev.Message != message
		conds.SetTrueWithReason(v1alpha1.ConditionTypeSSHKeysReady, "SSHKeysNarrowed", message)
		if firstTime {
			s.recorder.Publish(SSHKeysNarrowedEvent(nodeClass, message))
		}
		return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
	}

	conds.SetTrue(v1alpha1.ConditionTypeSSHKeysReady)
	return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
}
