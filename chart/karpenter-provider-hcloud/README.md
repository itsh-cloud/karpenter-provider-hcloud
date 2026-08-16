# karpenter-provider-hcloud

Helm chart for the Karpenter cloud provider for Hetzner Cloud.

## Install

The chart does not template the Hetzner credential. Create the Secret first, so
the token never passes through a values file, the Helm release Secret, or
`helm get values`:

```bash
kubectl create namespace karpenter
kubectl create secret generic hcloud -n karpenter --from-literal=token=<project-token>
```

The token needs **read-write** on the project's servers.

```bash
helm install karpenter-provider-hcloud oci://ghcr.io/itsh-cloud/charts/karpenter-provider-hcloud \
  --version 0.1.0-alpha.3 --namespace karpenter
```

Installing with no NodePool is inert: the controller resolves and reports every
HCloudNodeClass, and provisions nothing until a NodePool references one.

## Status

Pre-release. The NodeClass controllers are complete: selectors resolve against
the Hetzner API into `.status`, conditions roll up into `Ready`, the spec hash
drift compares against is maintained, and a finalizer holds a class open while
NodeClaims reference it.

`CloudProvider` Create, Delete, Get and List are implemented, along with a
garbage collector for servers whose NodeClaim has gone away. A NodePool
referencing a ready NodeClass will provision nodes.

Drift detection and node repair are deliberately still off. Everything else
karpenter core ships is ON, including consolidation (whose NodePool defaults
are `WhenEmptyOrUnderutilized` with `consolidateAfter: 0s`), expiration, and
drain-and-evict termination. A NodePool that should not disrupt anything yet
must say so itself with `spec.disruption.budgets: [{nodes: "0"}]`.

## What it grants, and why

Every rule in `templates/rbac.yaml` is derived from a call site, and the
comments there say which. Three are worth knowing before you read the file:

- **Cluster-wide read on pods, nodes and volumeattachments** is not this
  provider's choice. Karpenter's operator starts field indexers over those types
  at construction, and the manager blocks in `WaitForCacheSync` until each one
  syncs. Removing them does not reduce the blast radius, it stops the pod
  serving `/readyz` with an error that names no resource.
- **`patch` on `nodeclaims`** is required by the spec-hash back-fill. Without
  it, a hash generator change leaves every NodeClaim holding a hash the new
  generator did not produce, and drift reads the whole fleet as out of date.
- **Secrets in `kube-system` are `create` and `delete`, with no read.** That
  absence is a security control. With read, the grant is a path to
  cluster-admin: create a `kubernetes.io/service-account-token` Secret naming a
  privileged ServiceAccount, wait for the token controller to fill it in, then
  read the token back. Nothing here needs to read a Secret, so nothing may.

## Requirements that are not tuning knobs

- `dnsConfig.options.ndots=1`. If you run the egress policy from this project,
  DNS is permitted for exactly one name. At the cluster default of `ndots:5` the
  resolver treats `api.hetzner.cloud` as partial and tries the search domains
  first, producing lookups the DNS proxy denies. Cilium's `matchPattern` cannot
  paper over this: its wildcard matches within a single label.
- **The controller must not run on nodes it manages.** The default
  `nodeSelector` and `tolerations` place it on control plane nodes, which are
  managed by your own infrastructure code rather than by this provider. On a
  node it manages, it can be consolidated away by itself, or be the pod being
  drained from the node whose termination it is supposed to be completing.
- **In-cluster config only.** No kubeconfig is mounted and none is read.

## CRDs

The chart ships three CRDs: its own `HCloudNodeClass`, and `NodePool` and
`NodeClaim` copied from the pinned `sigs.k8s.io/karpenter` module so they cannot
drift from the schema the binary is compiled against.

They are rendered as ordinary templates, so `helm upgrade` applies schema
changes, and they carry `helm.sh/resource-policy: keep`, so `helm uninstall`
leaves them and the HCloudNodeClasses they hold. Set `crds.enabled=false` if you
apply them separately.

## Values

See `values.yaml`; the comments there explain the non-obvious defaults.
