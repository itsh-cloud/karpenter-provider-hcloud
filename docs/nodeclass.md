# HCloudNodeClass

Cluster-scoped. Short name `hcnc`. One NodeClass describes how to build a node; NodePools
reference it and narrow it with requirements.

`spec.imageSelector` and `spec.bootstrap.kubernetesVersion` are the only required fields.

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
    - name: k8s-fw
  sshKeySelectors:
    - name: ops
  bootstrap:
    kubernetesVersion: "1.34.7"
```

## Selectors

Every selector takes a `name` **or** an `id`. Names are resolved once by the controller and
published as ids in `.status`, so a create never races a selector that would resolve
differently between the decision and the order.

| Field | Required | Notes |
|---|---|---|
| `imageSelector` | yes | Name lookups are architecture-qualified. An `id` is not, so a mismatched architecture is caught by validation instead. |
| `networkSelector` | no | The network's zone is a hard bound: a server cannot attach to a network in another zone, so locations outside it are dropped. |
| `firewallSelectors` | no | Fails **closed**. A partially resolved set is published as none, because launching with a subset silently opens whatever the missing one closed. |
| `sshKeySelectors` | no | Fails **open**. Nothing about joining needs SSH, so a key deleted from the project narrows the set with a warning rather than halting every NodePool. A rejected credential still fails closed. |
| `placementGroup` | no | Hetzner caps spread groups at 10 members; the eleventh create fails with `placement_error`, which is indistinguishable from a stockout. The condition warns from 8. |

## locations

Bounds which Hetzner locations this class may use. Left unset, every location the catalog
offers (within the network's zone, if any) is permitted.

**Set it explicitly if you want location drift.** Drift only treats a location as removed
when you wrote the list down: left to the catalog, a partial API response or a type going
deprecated in one location would silently narrow the set and drift every node there.

`locations` is not part of the spec hash, so editing it does not roll the fleet by itself.

## bootstrap

Debian, kubeadm and containerd. The join configuration is modelled rather than templated,
which is what makes per-NodeClaim tokens, correct taint rendering and a single source of
truth for kubelet reservations possible.

| Field | Notes |
|---|---|
| `kubernetesVersion` | Required, full version, e.g. `1.34.7`. |
| `packageRevision` | Debian revision suffix. Pin it to make a build reproducible. |
| `extraPackages`, `kernelModules`, `sysctls`, `extraFiles` | The escape hatches. `extraFiles` are written before `runcmd`, so they can configure something a later command uses. |
| `preJoinCommands`, `postJoinCommands` | Run around `kubeadm join`. |
| `apiServerEndpoint`, `caCertHashes` | Override discovery. Set **both** or neither: half a pair produces a server that boots and never joins. Normally read from the `kube-public/cluster-info` ConfigMap. |
| `revision` | Has no effect except that it is hashed, so bumping it drifts every node deliberately. |

Installing a second runtime is composition, not a feature: install it with `extraPackages`
and `extraFiles`, then advertise it with an ordinary NodePool template label.

## kubelet

The authoritative source for reservations. The same numbers feed the rendered join
configuration **and** Karpenter's allocatable calculation. If they diverge, Karpenter
over-packs every node by the difference and nothing reports it, which is why there is one
field rather than two.

`maxPods` defaults to 110 and is also the `pods` capacity Karpenter schedules against.

## imageDriftPolicy

`Ignore` (default) or `Replace`.

Hetzner rebuilds its named images every few weeks, so the id behind a name changes on
Hetzner's schedule rather than yours. Under `Ignore` that is not drift. `Replace` is for
people who want their fleet rolled whenever Hetzner publishes.

## What drifts a node

Two mechanisms, deliberately separate:

- **The spec hash**, covering everything Hetzner does not return on a read: `bootstrap`,
  `kubelet`, `sshKeySelectors`, `serverLabels`, `revision`.
- **A live comparison** for what it does return: location, network, firewalls, placement
  group, public net, and image under `Replace`.

The server **type** is never drift. Which shape a workload sits on is a scheduling decision
and correcting it is consolidation's job; treating it as drift would have the two fighting
over the same node.

Hash comparison is skipped entirely unless the NodeClass and the NodeClaim agree on the hash
*version*, because a changed hash generator produces a different hash for an unchanged spec
and comparing across versions would replace the whole fleet at once.

## Status

`.status` carries the resolved ids, the usable locations, and the discovered join
parameters. Conditions roll up into `Ready`, which is what Karpenter gates provisioning on.

A **configuration** failure (resource genuinely absent, invalid input, rejected token) sets
the relevant condition `False` and names the selector. A **transient** failure leaves
conditions untouched and retries, because reporting a blip as configuration would take every
NodePool using the class out of service over a few seconds of API trouble.

See [troubleshooting.md](troubleshooting.md) for reading these in anger.
