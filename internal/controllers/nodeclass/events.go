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
// explanation, which reads like a stuck controller. Deduped by UID, and
// re-published each pass so it does not age out of the TTL while still true.
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
// A Warning rather than Normal: nothing fails, but the capacity an operator
// thinks they have is smaller than they wrote down, and a condition on an object
// nobody is looking at is not a notification.
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
// A Warning, and the only unprompted notice of it. hcloud does not return
// ssh_keys on a server GET, so the reduced key set is not discoverable from the
// API: the first symptom is a session that will not open.
func SSHKeysNarrowedEvent(nodeClass *v1alpha1.HCloudNodeClass, message string) events.Event {
	return events.Event{
		InvolvedObject: nodeClass,
		Type:           corev1.EventTypeWarning,
		Reason:         "SSHKeysNarrowed",
		Message:        message,
		DedupeValues:   []string{string(nodeClass.UID)},
	}
}
