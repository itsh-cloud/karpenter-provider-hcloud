# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-08-16

First release. The provider provisions, replaces and terminates nodes against a real cluster
and a real Hetzner project.

### Added

- **CloudProvider**: Create, Delete, Get, List, GetInstanceTypes, IsDrifted, RepairPolicies,
  GetSupportedNodeClasses. Create re-derives its own price ordering rather than trusting the
  order Karpenter passes in, because Karpenter computes one and then loses it through a Go map;
  a provider that walks that order picks at random, which is the failure this project exists to
  prevent.
- **Capacity fall-through.** A create that fails with any capacity-class error falls through to
  the next-cheapest candidate and records the failed (server type, location) pair so the next
  pass skips it. Rate limiting is explicitly not a capacity signal: treating it as one converts
  throttling into a self-inflicted outage.
- **Ownership model.** Every server carries `karpenter.sh/managed-by: <clusterName>`, checked on
  read as well as delete and failing closed. Terraform-created control plane servers carry no
  such label, which is what makes the check meaningful. `clusterName` has no default.
- **Drift**: the NodeClass spec hash (gated on both sides agreeing on the hash version) plus a
  live comparison of location, network, firewalls, placement group, public net and image. The
  server type is deliberately never a drift reason.
- **Garbage collection** for servers whose NodeClaim has gone away, floored at five minutes and
  refusing to act when there are no NodeClaims at all.
- **Per-NodeClaim bootstrap tokens** with a short TTL, owned by the NodeClaim so they are
  collected with it. No long-lived shared join token.
- **Metrics** for launch outcomes and duration, offering suppression, Hetzner's published
  availability flag (exported, and used for nothing), catalog freshness and unrecognised error
  codes.
- **Helm chart** with least-privilege RBAC derived from call sites, and an admission policy
  denying the provider's ServiceAccount deletion of control plane nodes.

### Notes

- Offerings declare `topology.kubernetes.io/zone`. Without it Karpenter cannot price a running
  node, every candidate prices at zero, and replacement consolidation can never fire. The value
  is derived the same way hcloud-cloud-controller-manager derives the label, from the location,
  so the two agree by construction.
- Hetzner has no spot capacity. Every NodePool must pin
  `karpenter.sh/capacity-type: [on-demand]`, or consolidation pins replacements to spot and
  retries forever.

### Known limitations

- x86/amd64 only.
- Debian, kubeadm and containerd only.
- Node repair is implemented but inert unless the `NodeRepair` feature gate is enabled.

### Added

- Repository skeleton: build tooling, linting, CI, and a bare operator binary that starts
  `sigs.k8s.io/karpenter`'s manager with no controllers registered.
- Pinned `sigs.k8s.io/karpenter` v1.14.0. The pin is exact, and upstream minor bumps are
  reviewed by hand rather than auto-merged, because they have historically changed
  provider-facing interfaces.
- `HCloudNodeClass` status controller: resolves every selector against the Hetzner API,
  publishes the resolved identifiers and the usable locations, and rolls the per-resource
  conditions up into `Ready`, which is what Karpenter gates provisioning on. A missing
  resource is reported as a `False` condition naming the selector; a failed API call is
  retried and leaves the condition alone, so a blip cannot take a healthy class down.
- `HCloudNodeClass` hash controller, including the hash-version back-fill that keeps a
  change to the hash generator from marking the whole fleet drifted at once.
- Termination finalizer holding an `HCloudNodeClass` open while NodeClaims still reference
  it. Karpenter core neither blocks nor cascades here, so without it a delete leaves nodes
  running against a class that no longer exists.
- `HCloudNodeClass` and `HCloudNodeClassList` are registered into client-go's global scheme.
  `operatorpkg`'s GVK lookup resolves through that scheme and panics on an unknown type, so
  the omission would have killed the binary at startup rather than failing a reconcile.
