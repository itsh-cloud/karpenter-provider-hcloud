package instancetype

// LegacyDatacenterForLocation returns the value hcloud-CCM writes into
// topology.kubernetes.io/zone for a server in this location.
//
// The zone must be declared and must be right. Karpenter prices a running node
// by matching an offering against the node's zone label; with no match the
// price resolves to 0, consolidation then filters replacements to those cheaper
// than 0, finds none, and reports "Can't replace with a cheaper node" forever.
// Leaving zone unconstrained does NOT avoid this: Requirements.Get on a missing
// key returns an Exists requirement whose Any() is a RANDOM number.
//
// The table mirrors hcloud-cloud-controller-manager's
// internal/legacydatacenter.NameFromLocation (v1.33.0). That function writes the
// label and is a PURE FUNCTION OF THE LOCATION, never reading the server's real
// datacenter, so the zone is predictable without an API call and an unknown
// location maps to itself exactly as the CCM does.
//
// The risk is not this table going stale. Upstream recommends disabling the
// label (HCLOUD_INSTANCES_ZONE_LABEL_ENABLED=false) and plans to remove it,
// which breaks the price lookup just as thoroughly as a wrong value here. Alert
// on the symptom: a node whose zone label is absent or disagrees with this
// function is the state in which consolidation silently stops working.
func LegacyDatacenterForLocation(location string) string {
	switch location {
	case "nbg1":
		return "nbg1-dc3"
	case "hel1":
		return "hel1-dc2"
	case "fsn1":
		return "fsn1-dc14"
	case "ash":
		return "ash-dc1"
	case "hil":
		return "hil-dc1"
	case "sin":
		return "sin-dc1"
	default:
		return location
	}
}
