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
  --version 0.2.0 --namespace karpenter --set clusterName=<cluster-name>
```

`clusterName` is required. It becomes the `karpenter.sh/managed-by` label on every server, and
every destructive path checks it, so two clusters sharing a Hetzner project must use different
names.

Installing with no NodePool is inert: the controller resolves and reports every
HCloudNodeClass, and provisions nothing until a NodePool references one.

## Status

Alpha. It provisions nodes, replaces them on consolidation and drift, terminates them, and
garbage-collects servers whose NodeClaim has gone away.

Drift detection is ON. Node repair is implemented but inert unless the `NodeRepair` feature
gate is enabled. Consolidation, expiration and drain-and-evict termination are all on by
default, so a NodePool that should not disrupt anything yet must say so itself with
`spec.disruption.budgets: [{nodes: "0"}]`.

**Every NodePool must pin `karpenter.sh/capacity-type: [on-demand]`.** Hetzner has no spot
capacity, but an unconstrained requirement is unbounded, so Karpenter pins consolidation
replacements to spot and retries the failed launch indefinitely.

## Feature gates

Set karpenter core feature gates through `controller.featureGates`, as a map of gate name to
`true` or `false`:

```yaml
controller:
  featureGates:
    NodeRepair: true
```

The chart renders them into `FEATURE_GATES` and grants what a gate needs from the same map,
so rendering fails if a gate is set any other way (`--feature-gates` in
`controller.extraArgs`, or `FEATURE_GATES` in `controller.extraEnv`).

Enabling `PodDeletionCostManagement` also grants `patch` and `update` on pods cluster-wide.
That is an escalation path, since a pod's container image is mutable and patching it runs
chosen code under that pod's ServiceAccount, which is why the grant is withheld unless the
gate is on.

## What it grants, and why

Every rule in `templates/rbac.yaml` (this provider) and `templates/rbac-core.yaml`
(karpenter core) is derived from a call site, and the comments there say which. Three are
worth knowing before you read the files:

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

- `dnsConfig.options.ndots=1`. The only external name the controller resolves
  is `api.hetzner.cloud`. At the cluster default of `ndots:5` the resolver
  treats it as partial and tries every search domain first, which an egress
  policy allowlisting DNS by name would deny.
- **The controller must not run on nodes it manages.** The default
  `nodeSelector` and `tolerations` place it on control plane nodes, which are
  managed by your own infrastructure code rather than by this provider. On a
  node it manages, it can be consolidated away by itself, or be the pod being
  drained from the node whose termination it is supposed to be completing.
- **In-cluster config only.** No kubeconfig is mounted and none is read.

## Metrics

`serviceMonitor.enabled=true` creates a ServiceMonitor for the Prometheus Operator. Set
`serviceMonitor.additionalLabels` to whatever your Prometheus's `serviceMonitorSelector`
matches (for kube-prometheus-stack, usually `release: <its release name>`), or it is created
and scraped by nothing.

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
