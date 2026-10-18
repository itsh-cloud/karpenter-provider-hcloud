# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.1] - 2026-10-18

### Added

- Instance types declare Hetzner's limit of 16 attached volumes for `csi.hetzner.cloud`, so
  Karpenter no longer plans more volume-backed pods onto a new node than the node can attach.

## [0.3.0] - 2026-10-18

### Changed

- Built against karpenter core v1.15.0, with `k8s.io/*`, controller-runtime and operatorpkg on
  the versions core v1.15.0 builds with, and hcloud-go v2.52.0. The karpenter.sh CRDs carry
  the v1.15 schema: the `Unhealthy` disruption budget reason, and `consolidateAfter`
  defaulting to `0s` instead of being required. Both are additive, and the upgrade drifts no
  node.
- **Breaking (chart):** karpenter core feature gates are set through
  `controller.featureGates`. Rendering fails on `--feature-gates` in `controller.extraArgs`
  or `FEATURE_GATES` in `controller.extraEnv`, because the chart grants what a gate needs from
  that map.
- **Breaking (chart):** `serviceMonitor.additionalLabels` defaults to `{}` instead of
  `release: kube-prometheus`. Set it to whatever your Prometheus's `serviceMonitorSelector`
  matches.
- The chart requires Kubernetes 1.30, where the ValidatingAdmissionPolicy it renders by
  default is GA.
- Repair policies use the v1.15 form. Ready=False and Ready=Unknown each replace the node
  after 30 minutes without draining, as before. With `NodeRepair` on, the policy set is now
  validated at startup.
- `Reboot` reports in-place reboot as not implemented, so node repair always replaces.
- Core's ClusterRole reads `services`, which core lists when `SCHEDULER_CONFIG` sets default
  topology spread constraints.
- `v1alpha1.SchemeBuilder` is apimachinery's `runtime.SchemeBuilder` instead of
  controller-runtime's deprecated `scheme.Builder`. `v1alpha1.AddToScheme` is unchanged.

### Added

- `controller.featureGates`, a map of core gate name to bool. Enabling
  `PodDeletionCostManagement` there also grants pods `patch` and `update`, which is withheld
  otherwise because it is an escalation path.

### Fixed

- SECURITY.md said the controller could `get` Secrets by name. It holds only `create` and
  `delete`.
- The troubleshooting guide no longer attributes duplicate nodes to pod nomination, which
  only gates disruption, and its zone label section describes the label this provider sets.

## [0.2.0] - 2026-10-01

### Added

- `spec.maxNodeAge` on HCloudNodeClass. A node whose NodeClaim is older than it drifts with
  reason `NodeAgeDrift`, so nodes are replaced on a schedule inside the disruption budget and
  respecting PodDisruptionBudgets, which a NodePool's `expireAfter` does not. Unset by default,
  minimum `1h`. It is not part of the spec hash, so setting it does not roll the fleet by
  itself, but nodes already older than the new value drift together and the budget paces them.
- Hetzner API request metrics from hcloud-go: `hcloud_api_requests_total` (by code, method and
  endpoint, each retry counted), `hcloud_api_request_duration_seconds` and
  `hcloud_api_in_flight_requests`. A non-zero `code="429"` rate is the rate limit being hit.

### Fixed

- A stale list cache no longer sends `Get` to the API once per server. Past the 30s TTL, every
  NodeClaim's drift check read its server individually, which on a busy cluster still
  exhausted the rate limit. `Get` now refreshes the listing through the same single
  flight `List` uses, so a drift pass costs one listing, and a failed listing is returned as an
  error rather than retried per server. That shared listing now has its own 60s deadline, and a
  caller whose context ends stops waiting without cancelling it for the others.
- The Hetzner API token is redacted from errors. Hetzner's rate-limit message quotes the first
  half of the token, and it reached logs, events, NodeClaim errors and NodeClass conditions.
  The token is now removed from Hetzner's message text by value, whatever the wording, and
  errors no longer carry the HTTP response, whose request holds the token in full.
- Nodes run the kernel the boot-time package upgrade installed. With `packageUpgradeOnBoot`
  (the default), cloud-init now reboots before joining when the upgrade requires it, where a
  new node used to run the image's older kernel with a reboot pending for its whole life. Boots
  that take the reboot are about a minute longer. The change is in rendered user data, not the
  spec hash, so it drifts no existing node.

## [0.1.1] - 2026-08-28

### Fixed

- `Get` is served from the existing 30s list cache instead of calling the Hetzner API every
  time. Drift calls it once per NodeClaim per reconcile and its controller watches Pods
  unfiltered, so on a busy cluster this was a large share of the 3600 requests/hour a project
  allows. A cache miss still reads live, because absence from a listing is not absence of the
  server.

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
  read as well as delete and failing closed. Control plane servers provisioned outside the
  provider carry no such label, which is what makes the check meaningful. `clusterName` has no default.
- **Drift**: the NodeClass spec hash (gated on both sides agreeing on the hash version) plus a
  live comparison of location, network, firewalls, placement group, public net and image. The
  server type is deliberately never a drift reason.
- **Garbage collection** for servers whose NodeClaim has gone away, floored at five minutes and
  refusing to act when there are no NodeClaims at all.
- **Per-NodeClaim bootstrap tokens** with a short TTL, owned by the NodeClaim so they are
  collected with it.
- **Metrics** for launch outcomes and duration, offering suppression, Hetzner's published
  availability flag (exported, and used for nothing), catalog freshness and unrecognised error
  codes.
- **HCloudNodeClass status**: every selector is resolved against the Hetzner API, the
  resolved identifiers and usable locations are published, and the per-resource conditions
  roll up into `Ready`, which is what Karpenter gates provisioning on. A missing resource is
  reported as a `False` condition naming the selector; a failed API call is retried and
  leaves the condition alone, so a blip cannot take a healthy class down.
- A **termination finalizer** holds an `HCloudNodeClass` open while NodeClaims still
  reference it. Karpenter core neither blocks nor cascades here, so without it a delete
  leaves nodes running against a class that no longer exists.
- **Helm chart** with least-privilege RBAC derived from call sites, and an admission policy
  denying the provider's ServiceAccount deletion of control plane nodes.
- Built against `sigs.k8s.io/karpenter` v1.14.0, pinned exactly. Upstream minor bumps are
  reviewed by hand rather than auto-merged, because they have changed provider-facing
  interfaces.

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

[Unreleased]: https://github.com/itsh-cloud/karpenter-provider-hcloud/compare/v0.3.1...HEAD
[0.3.1]: https://github.com/itsh-cloud/karpenter-provider-hcloud/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/itsh-cloud/karpenter-provider-hcloud/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/itsh-cloud/karpenter-provider-hcloud/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/itsh-cloud/karpenter-provider-hcloud/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/itsh-cloud/karpenter-provider-hcloud/releases/tag/v0.1.0
