# karpenter-provider-hcloud

A [Karpenter](https://karpenter.sh) cloud provider for [Hetzner Cloud](https://www.hetzner.com/cloud).

> **Status: alpha.** Provisions, consolidates and terminates nodes. The API may still
> change. See [Project status](#project-status).

## Why

Cluster-autoscaler has exactly one disruption move: delete an underutilized node. It has no
concept of *"this node is the wrong type, replace it"*.

That matters on Hetzner, where server types go out of stock in a location for days at a
time. When your preferred type is unavailable, cluster-autoscaler falls back to whatever it
can order and then **never undoes it**: the substitute fills up, stops being underutilized,
and becomes permanent. Karpenter's consolidation controller can *replace* a node with a
cheaper or better-fitting one, not merely remove idle ones. That is the capability this
provider brings to Hetzner.

All the hard parts (provisioning, bin-packing, consolidation, drift, disruption budgets)
live in [`sigs.k8s.io/karpenter`](https://github.com/kubernetes-sigs/karpenter). This
repository supplies the nine-method `CloudProvider` interface and the NodeClass CRD that
describe how to build a Hetzner node.

## Scope and opinions

- **Debian + kubeadm + containerd only.** The kubeadm join and containerd configuration are
  first-class CRD fields rather than an opaque `userData` blob, which is what makes
  per-NodeClaim bootstrap tokens, correct taint rendering and a single source of truth for
  kubelet reservations possible. Talos, k3s and non-Debian images are not supported in v1.
- **x86/amd64 only** for now.
- **Bootstrap tokens are minted per NodeClaim** with a short TTL, owned by the NodeClaim so
  they are garbage-collected with it. There is no long-lived shared join token.
- **No opinion beyond that baseline.** Extra runtimes, drivers and kernel modules go in
  `extraPackages`, `extraFiles` and the join hooks, and are advertised to the scheduler with
  ordinary NodePool template labels. A `Custom` bootstrap mode taking a templated `userData`
  is the planned route to other distributions; the `osFamily` and `mode` enums are
  single-valued today so that addition is additive.

## Quick start

Requires a kubeadm-built cluster, hcloud-cloud-controller-manager, and a Hetzner project
token with read-write on servers.

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
fails forever in a retry loop. This is the most common way to misconfigure this provider.

## Project status

Alpha, exercised against a real cluster and a real Hetzner project. It provisions nodes,
replaces them on consolidation and drift, terminates them, and garbage-collects servers
whose NodeClaim has gone away.

Not yet done: ARM, non-Debian images, a `Custom` bootstrap mode, and IPv6-only nodes. The
CRD is `v1alpha1` and may change. Pin a tag.

## Development

Requires Go (see `go.mod`) and [`just`](https://github.com/casey/just).

```bash
just build      # build the binary
just test       # run tests
just lint       # golangci-lint
just vet        # go vet
just generate   # regenerate CRDs and deepcopy (needs controller-gen)
```

`just generate-docker` runs the generation step in a container if you would rather not
install `controller-gen` locally.

`just e2e` runs tests against a **real Hetzner project**, creating and destroying servers. It
costs money, needs a scratch project, and is never run in CI.

## Documentation

- [docs/nodeclass.md](docs/nodeclass.md): the `HCloudNodeClass` API
- [docs/troubleshooting.md](docs/troubleshooting.md): failure modes and how to read them
- [chart/karpenter-provider-hcloud/README.md](chart/karpenter-provider-hcloud/README.md):
  chart values, and what the RBAC grants and why

## License

Apache 2.0. See [LICENSE](LICENSE).
