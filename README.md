# karpenter-provider-hcloud

A [Karpenter](https://karpenter.sh) cloud provider for [Hetzner Cloud](https://www.hetzner.com/cloud).

> **Status: pre-alpha.** Not yet usable. See [Project status](#project-status).

## Why

Cluster-autoscaler has exactly one disruption move: delete an underutilized node. It has no
concept of *"this node is the wrong type, replace it"*.

That matters on Hetzner, where server types go out of stock in a location for days at a time.
When your preferred type is unavailable, cluster-autoscaler falls back to whatever it can order
and then **never undoes it** — the substitute node fills up, stops being underutilized, and
becomes permanent. Fleets accumulate expensive fallback nodes that outlive the outage that
caused them by weeks.

Karpenter's consolidation controller can *replace* a node with a cheaper or better-fitting one,
not merely remove idle ones. That is the capability this provider exists to bring to Hetzner.

## What this is

Karpenter splits into a core library and per-cloud providers. All the hard parts —
provisioning, bin-packing, consolidation, drift, disruption budgets — live in
[`sigs.k8s.io/karpenter`](https://github.com/kubernetes-sigs/karpenter). A provider supplies a
nine-method `CloudProvider` interface and a NodeClass CRD describing how to build a node.

This repository is the Hetzner Cloud implementation of that interface.

## Scope and opinions

This provider is deliberately opinionated, and the constraints are worth knowing before you
adopt it:

- **Debian + kubeadm + containerd only.** The kubeadm join and containerd configuration are
  first-class CRD fields rather than an opaque `userData` blob. That is what makes
  per-NodeClaim bootstrap tokens, correct taint rendering, and a single source of truth for
  kubelet reservations possible. It also means Talos, k3s and non-Debian images are not
  supported in v1.
- **No opinion beyond that baseline.** Extra runtimes, drivers and kernel modules are
  expressed with `extraPackages`, `extraFiles` and the join hooks, and advertised to the
  scheduler with ordinary NodePool template labels. Karpenter already schedules against
  `nodeSelector` natively, so a node capability needs no provider support to be selectable,
  only to be installed.
  A `Custom` bootstrap mode, taking a templated `userData` with the join token, endpoint, CA
  hashes, taints and node labels injected as variables, is the planned way to support other
  distributions and join mechanisms. The `osFamily` and `mode` enums are single-valued today
  specifically so that addition is additive.
- **x86/amd64 only** for now.
- **Bootstrap tokens are minted per NodeClaim with a short TTL**, owned by the NodeClaim so
  they are garbage-collected when it goes away. There is no long-lived shared join token.

## Project status

Under active initial development. The interface is not stable, nothing is released, and it
should not be pointed at a cluster you care about.

Pin a released tag once one exists.

## Development

Requires Go (see `go.mod`) and [`just`](https://github.com/casey/just).

```bash
just build      # build the binary
just test       # run tests
just lint       # golangci-lint
just vet        # go vet
just generate   # regenerate CRDs and deepcopy (needs controller-gen)
```

`just generate-docker` runs the generation step in a container if you would rather not install
`controller-gen` locally.

`just e2e` runs tests against a **real Hetzner project**, creating and destroying servers. It
costs money, needs a scratch project, and is never run in CI.

## License

Apache 2.0. See [LICENSE](LICENSE).
