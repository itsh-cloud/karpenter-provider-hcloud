package instancetype

// LegacyDatacenterForLocation returns the value hcloud-CCM writes into
// topology.kubernetes.io/zone for a server in this location.
//
// # Why this exists and why it is a hardcoded table
//
// Karpenter core prices a running node with
// InstanceType.OfferingPrice(node.Labels()["topology.kubernetes.io/zone"], ...),
// which matches an Offering by its zone requirement. If no offering carries the
// node's zone, the lookup fails, resolveNodePrice returns 0, and consolidation
// then filters replacements to those cheaper than 0, i.e. none. The symptom is
// core reporting "Can't replace with a cheaper node" for every candidate
// forever, which silently removes the one capability this whole project exists
// to gain: replacing a correctly-utilised but wrongly-typed node.
//
// Leaving zone unconstrained does NOT avoid that. Requirements.Get on a missing
// key returns an Exists requirement, and Any() on Exists returns a RANDOM
// number, so Offering.Zone() yields a different nonsense string every call and
// can never equal a real label.
//
// The table mirrors hcloud-cloud-controller-manager's own
// internal/legacydatacenter.NameFromLocation (v1.33.0, unchanged on main). That
// function is the authority here, because it is what actually writes the label,
// and it is a PURE FUNCTION OF THE LOCATION: the CCM never reads the server's
// real datacenter. So the zone is fully predictable, and this provider can
// declare it without guessing.
//
// This is a deliberate reversal of the earlier decision to leave zone free,
// which was made on the belief that the label reflected the datacenter Hetzner
// happened to place the server in and therefore could not be predicted. It does
// not, and it can.
//
// Deliberately NOT read from Hetzner's datacenters API: the entire Datacenter
// type is removed after 2026-10-01, and this needs no API call at all.
//
// The default mirrors the CCM's: a location the table does not know maps to
// itself, which is also what the CCM's comment promises for new locations.
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
