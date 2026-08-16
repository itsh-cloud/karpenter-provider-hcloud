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
// # Why a missing key does not stop provisioning
//
// Unlike every other selector here, this one fails OPEN when a key simply is
// not there. Nothing about joining needs SSH: kubeadm joins over the network,
// and the provider never opens a session. So a key that has been deleted from
// the Hetzner project, typically because someone pruned a departed colleague,
// costs an operator the ability to log in to nodes built afterwards. Failing
// closed on it costs them every NodePool that uses the class, cluster-wide,
// within one requeue, and turns an act of good hygiene into an outage.
//
// A rejected or read-only token still fails closed. That is not a statement
// about SSH keys at all, it means nothing on this NodeClass can be trusted to
// have resolved, and it is going to fail the other five conditions in the same
// pass regardless.
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
		// No keys is a supported configuration, not an incomplete one: joining
		// is done by kubeadm over the network and the provider has no need for
		// SSH. So True, not Unknown.
		nodeClass.Status.SSHKeys = nil
		nodeClass.StatusConditions(status.WithClock(s.clk)).SetTrue(v1alpha1.ConditionTypeSSHKeysReady)
		return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
	}

	resolved := make([]v1alpha1.SSHKeyStatus, 0, len(nodeClass.Spec.SSHKeySelectors))
	var missing []string
	var reason string
	var cause error
	// Accumulated across the whole loop rather than read off the last failure.
	// Deciding fail-open versus fail-closed from a single overwritten variable
	// makes the decision depend on ITERATION ORDER: one missing key and one
	// rejected credential would fail closed or open according to which selector
	// happened to be listed last, and in the open direction it publishes a
	// partial key set as success while claiming the key no longer exists, when
	// in fact the API refused to answer.
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
		// True, with the loss stated three ways. It has to be loud: hcloud does
		// not return ssh_keys on a server GET, so a node built with a reduced
		// key set is not detectable from the API afterwards, and this condition
		// is the only place the fact exists.
		//
		// Published on transition only, for the same reason as the location
		// narrowing: this re-evaluates every five minutes and the recorder's
		// dedupe window is two.
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
