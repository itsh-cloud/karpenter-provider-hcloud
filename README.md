# karpenter-provider-hcloud

A [Karpenter](https://karpenter.sh) cloud provider for [Hetzner Cloud](https://www.hetzner.com/cloud).

> **Status: alpha.** Provisions, consolidates and terminates nodes. The API may still
> change. See [Project status](#project-status).

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

## Quick start

Requires a kubeadm-built cluster, hcloud-cloud-controller-manager, and a Hetzner project token
with read-write on servers.

```bash
kubectl create namespace karpenter
kubectl create secret generic hcloud -n karpenter --from-literal=token=<project-token>

helm install karpenter-provider-hcloud \
  oci://ghcr.io/itsh-cloud/charts/karpenter-provider-hcloud \
  --namespace karpenter --set clusterName=<your-cluster-name>
```

`clusterName` has no default on purpose: it becomes the ownership label on every server, and
every destructive path gates on it. Two clusters sharing one Hetzner project **must** use
different names or each will treat the other's nodes as its own to delete.

Then create an `HCloudNodeClass` and a `NodePool`. See [docs/nodeclass.md](docs/nodeclass.md)
for the full API, and [docs/troubleshooting.md](docs/troubleshooting.md) before you debug
anything.

### One thing that will bite you

Every NodePool must pin the capacity type:

```yaml
requirements:
  - key: karpenter.sh/capacity-type
    operator: In
    values: [on-demand]
```

Hetzner has no spot capacity, but an unconstrained requirement is *unbounded*, so Karpenter
concludes spot is permitted and pins every consolidation replacement to it. The launch then
fails forever in a retry loop. This is the single most common way to misconfigure this
provider, so [docs/troubleshooting.md](docs/troubleshooting.md) opens with it.

## Project status

Alpha. It provisions nodes, replaces them on consolidation and drift, terminates them, and
garbage-collects servers whose NodeClaim has gone away. It has been exercised against a real
cluster and a real Hetzner project.

Not yet done: ARM, non-Debian images, a `Custom` bootstrap mode, and IPv6-only nodes. The CRD
is `v1alpha1` and may change. Pin a tag.

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

## Documentation

- [docs/nodeclass.md](docs/nodeclass.md) — the `HCloudNodeClass` API
- [docs/troubleshooting.md](docs/troubleshooting.md) — failure modes and how to read them
- [chart/karpenter-provider-hcloud/README.md](chart/karpenter-provider-hcloud/README.md) — chart
  values, and what the RBAC grants and why

## License

Apache 2.0. See [LICENSE](LICENSE).
