# karpenter-provider-hcloud

[![CI](https://github.com/itsh-cloud/karpenter-provider-hcloud/actions/workflows/ci.yaml/badge.svg)](https://github.com/itsh-cloud/karpenter-provider-hcloud/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/release/itsh-cloud/karpenter-provider-hcloud?sort=semver)](https://github.com/itsh-cloud/karpenter-provider-hcloud/releases)
[![License](https://img.shields.io/github/license/itsh-cloud/karpenter-provider-hcloud)](LICENSE)

A [Karpenter](https://karpenter.sh) cloud provider for [Hetzner Cloud](https://www.hetzner.com/cloud).
It launches Hetzner servers for pending pods, joins them to a kubeadm cluster, and lets
Karpenter consolidate, replace and retire them.

Provisioning, bin-packing, consolidation, drift and disruption budgets live in
[`sigs.k8s.io/karpenter`](https://github.com/kubernetes-sigs/karpenter). This repository
implements its `CloudProvider` interface and the `HCloudNodeClass` CRD that describes how to
build a Hetzner node.

> **Alpha.** It provisions, consolidates, replaces and terminates nodes on real clusters. The
> `HCloudNodeClass` API is `v1alpha1` and may still change, so pin a version.

## Features

- **Price-ordered capacity fall-through.** A create walks the candidates cheapest first. A
  stockout on one (type, location) pair falls through to the next and suppresses that pair
  for five minutes, so the next pass skips it. Rate limiting is never mistaken for a
  stockout.
- **Replace-consolidation** with a price and zone on every offering, so Karpenter can
  price a running node and find the cheaper replacement. Hetzner server types go out of stock
  in a location for days at a time, so a node launched on a pricier fallback type can be
  replaced with the cheaper one once it is back.
- **Drift** from three separate sources: a hash of the NodeClass spec, a live comparison of
  location, network, firewalls, placement group, public net and image, and an optional
  `maxNodeAge` for replacement on a schedule inside the disruption budget.
- **Per-NodeClaim bootstrap tokens** with a short TTL, garbage-collected with their
  NodeClaim.
- **Ownership by label.** Every server carries `karpenter.sh/managed-by: <clusterName>`, and
  every read and delete checks it, failing closed. Servers whose NodeClaim has gone away are
  garbage-collected.
- **Metrics** for launch outcomes and latency, suppressed offerings, catalog freshness and
  every Hetzner API request.
- **A least-privilege Helm chart.** Every RBAC rule is tied to a call site, Secrets in
  `kube-system` are create and delete only, and an admission policy stops the controller
  deleting control plane nodes.

## Requirements

- A kubeadm-built cluster on Kubernetes 1.30 or newer, with containerd.
- [hcloud-cloud-controller-manager](https://github.com/hetznercloud/hcloud-cloud-controller-manager).
- Debian images for the nodes. Nodes are x86 only; the controller image is built for amd64
  and arm64.
- A Hetzner Cloud project API token with read-write access, ideally one used by nothing else.

## Compatibility

| Provider | Karpenter core | Kubernetes |
|---|---|---|
| 0.3.x | v1.15 | 1.30 to 1.36 |
| 0.1.x, 0.2.x | v1.14 | 1.30 to 1.36 |

The Kubernetes range is the one karpenter core is tested against.

## Install

The chart never templates the Hetzner token. Create the Secret first:

```bash
kubectl create namespace karpenter
kubectl create secret generic hcloud -n karpenter --from-literal=token=<project-token>

helm install karpenter-provider-hcloud \
  oci://ghcr.io/itsh-cloud/charts/karpenter-provider-hcloud \
  --namespace karpenter --set clusterName=<cluster-name>
```

`clusterName` has no default on purpose. It becomes the ownership label on every server, and
every destructive path checks it. Two clusters sharing one Hetzner project **must** use
different names, or each will treat the other's nodes as its own to delete.

The chart's [README](chart/karpenter-provider-hcloud/README.md) covers its values, what the
RBAC grants and why, and how to enable karpenter core feature gates.

## Example

An `HCloudNodeClass` says how to build a node. A `NodePool` says which nodes Karpenter may
launch from it.

```yaml
apiVersion: karpenter.itsh.dev/v1alpha1
kind: HCloudNodeClass
metadata:
  name: default
spec:
  imageSelector:
    name: debian-13
  locations: [nbg1, fsn1]
  networkSelector:
    name: k8s-network
  firewallSelectors:
    - name: k8s-nodes
  bootstrap:
    kubernetesVersion: "1.34.7" # match your control plane
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.itsh.dev
        kind: HCloudNodeClass
        name: default
      requirements:
        # Required on every NodePool, see below.
        - key: karpenter.sh/capacity-type
          operator: In
          values: [on-demand]
        - key: karpenter.itsh.dev/server-type-line
          operator: In
          values: [cx, cpx]
  limits:
    cpu: "64"
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 5m
```

**Every NodePool must pin `karpenter.sh/capacity-type: [on-demand]`.** Hetzner has no spot
capacity, but an unconstrained requirement is unbounded, so Karpenter concludes spot is
allowed and pins consolidation replacements to it. Those launches then fail and are retried
forever.

See [docs/nodeclass.md](docs/nodeclass.md) for the full `HCloudNodeClass` API, including
selectors, bootstrap options, kubelet reservations and drift.

## Limitations

- x86 nodes only.
- Debian, kubeadm and containerd only. There is no custom `userData` mode yet.
- No IPv6-only nodes.
- Node overlays are not applied, even with the `NodeOverlay` gate on and its CRD installed.
- Node repair is off unless the `NodeRepair` feature gate is enabled, and then it always
  replaces the node. In-place reboot is not implemented.

## Documentation

- [docs/nodeclass.md](docs/nodeclass.md): the `HCloudNodeClass` API
- [docs/troubleshooting.md](docs/troubleshooting.md): failure modes whose symptom points
  somewhere other than the cause
- [chart/karpenter-provider-hcloud/README.md](chart/karpenter-provider-hcloud/README.md): chart
  values, RBAC and feature gates
- [CHANGELOG.md](CHANGELOG.md)

## Development

Requires Go (see `go.mod`) and [`just`](https://github.com/casey/just).

```bash
just build             # build the binary
just test              # run the tests
just lint              # golangci-lint
just vet               # go vet
just generate-docker   # regenerate CRDs and deepcopy in a container
```

[CONTRIBUTING.md](CONTRIBUTING.md) has the conventions, and [SECURITY.md](SECURITY.md) the
threat model and how to report a vulnerability.

## License

Apache 2.0. See [LICENSE](LICENSE).

This project is not affiliated with or endorsed by Hetzner Online GmbH or the Karpenter
project.
