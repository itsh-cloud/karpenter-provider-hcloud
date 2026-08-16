package nodeclass

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

// WaitingOnNodeClaimTerminationEvent reports why a deleted HCloudNodeClass is
// not going away.
//
// Without it the object sits with a deletionTimestamp and a finalizer and no
// explanation anywhere, which reads exactly like a stuck controller. Deduped by
// UID so a NodeClass with fifty NodeClaims produces one event, and re-published
// on each pass so it does not age out of the event TTL while still true.
func WaitingOnNodeClaimTerminationEvent(nodeClass *v1alpha1.HCloudNodeClass, names []string) events.Event {
	return events.Event{
		InvolvedObject: nodeClass,
		Type:           corev1.EventTypeNormal,
		Reason:         "WaitingOnNodeClaimTermination",
		Message:        fmt.Sprintf("Waiting on NodeClaim termination for %s", pretty.Slice(names, 5)),
		DedupeValues:   []string{string(nodeClass.UID)},
	}
}

// LocationsNarrowedEvent reports that some of spec.locations cannot be used.
//
// A Warning rather than Normal: the class still works, so nothing fails, and a
// condition on an object nobody is looking at is not a notification. The
// capacity an operator thinks they have is smaller than they wrote down, and
// they find out either here or during an incident.
func LocationsNarrowedEvent(nodeClass *v1alpha1.HCloudNodeClass, message string) events.Event {
	return events.Event{
		InvolvedObject: nodeClass,
		Type:           corev1.EventTypeWarning,
		Reason:         "LocationsNarrowed",
		Message:        message,
		DedupeValues:   []string{string(nodeClass.UID)},
	}
}

// SSHKeysNarrowedEvent reports that a selected SSH key no longer exists and
// provisioning is continuing without it.
//
// A Warning, and the only unprompted notice of it. Nodes built from here on
// will not accept the missing key, and because hcloud does not return ssh_keys
// on a server GET, that is not discoverable from the API afterwards: the first
// symptom is a session that will not open, on a node whose peers are fine.
func SSHKeysNarrowedEvent(nodeClass *v1alpha1.HCloudNodeClass, message string) events.Event {
	return events.Event{
		InvolvedObject: nodeClass,
		Type:           corev1.EventTypeWarning,
		Reason:         "SSHKeysNarrowed",
		Message:        message,
		DedupeValues:   []string{string(nodeClass.UID)},
	}
}
