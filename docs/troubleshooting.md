# Troubleshooting

Failure modes that are hard to diagnose from the outside, in rough order of how often they
bite. Each one is written as *what you see*, then *what it actually is*, because in every case
below the symptom points somewhere other than the cause.

## Nothing consolidates, and NodeClaims churn every ~27 seconds

**Symptom.** A node is created, then a stream of NodeClaims appears and disappears. The node
itself never changes. `kubectl get nodeclaims` looks almost normal because each replacement
lives only seconds.

**Cause.** The NodePool does not constrain `karpenter.sh/capacity-type`.

An unconstrained requirement is *unbounded*, not empty, so Karpenter's test for "does this
permit both spot and on-demand" passes, and consolidation pins the replacement to spot.
Karpenter does this on purpose, expecting the launch to fail harmlessly and leave the node
alone. It does not leave it alone: it retries indefinitely. Hetzner has no spot capacity at
all, so the replacement can never launch.

**Fix.** Pin it on every NodePool:

```yaml
requirements:
  - key: karpenter.sh/capacity-type
    operator: In
    values: [on-demand]
```

**How to confirm** before changing anything: the disruption log names the replacement's
capacity type.

```bash
kubectl logs -n karpenter -l app.kubernetes.io/name=karpenter-provider-hcloud \
  | grep '"controller":"disruption"' | tail -3
```

A `"replacement-nodes":[{"capacity-type":"spot"...}]` on a cluster with no spot is this bug.

## A node is never replaced, however wrong its type

**Symptom.** An expensive or oversized node sits there forever. Consolidation events say
`Can't replace with a cheaper node` even though a cheaper type is clearly permitted and
available.

**Cause.** The node has no `topology.kubernetes.io/zone` label, or it disagrees with what this
provider puts on its offerings.

Karpenter prices a running node by finding an offering whose zone equals the node's zone label.
If none matches, the node prices at **0**, and nothing is ever cheaper than zero.

**How to confirm.**

```bash
kubectl get node <name> -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}'
```

Empty means hcloud-cloud-controller-manager has the zone label disabled
(`HCLOUD_INSTANCES_ZONE_LABEL_ENABLED=false`), which upstream now recommends for new clusters
and plans to make permanent. A value that is not this provider's mapping for the node's region
means the two have diverged. Either way the diagnosis is the CCM's configuration, not this
provider's decision-making.

## Pods with an existing volume are permanently unschedulable

**Symptom.** New pods schedule fine. Pods that mount an existing hcloud PersistentVolume never
schedule, and the scheduler blames node affinity.

**Cause.** Karpenter injects a bound volume's `nodeAffinity` keys as NodeClaim requirements,
and its compatibility check **denies** a custom label that a NodePool leaves undefined. If the
NodePool template does not define `csi.hetzner.cloud/location`, every such pod is rejected
before any node is considered.

**Fix.** This provider puts that key on every offering, so it works out of the box. If you
override requirements on the NodePool template, do not drop it.

## Nodes join but pods land before Karpenter has labelled them

**Symptom.** Bin-packing decisions look wrong for the first few seconds of a node's life.

**Cause.** The node registered without `karpenter.sh/unregistered:NoExecute`. Karpenter logs an
error and carries on, so the consequence is quiet rather than obvious.

**Fix.** This provider renders that taint into the kubeadm join configuration itself. If you
supply a custom bootstrap, you must render it too.

## A NodeClass sits at `Ready=False` and nothing provisions

Read the conditions rather than the roll-up. The roll-up names *which* dependency is failing,
and each dependency carries its own reason and message:

```bash
kubectl get hcloudnodeclass <name> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

- `...CredentialRejected` on several conditions at once means the token, not the selectors.
  The message says so explicitly.
- `...NotFound` names the selector that resolved to nothing.
- `CatalogNotFetched` or `CatalogEmpty` is `Unknown`, not `False`, and is about the Hetzner API
  rather than your configuration.

A transient API failure deliberately leaves conditions **untouched** rather than setting them
`Unknown`, so a healthy class keeps working through a Hetzner blip. If a condition is stuck at
`Unknown` with a `...Unreachable` reason, the API has never been reachable for that resource.

## One pending pod produces several nodes

**Symptom.** A single unschedulable pod results in two or more nodes, bounded only by the
NodePool's limits.

**Cause.** Karpenter nominates a pod to an in-flight NodeClaim for `max(2 × batch-max-duration,
10s)`, which is **20 seconds** by default. Nodes here take substantially longer than that to
boot, join and become schedulable, so the nomination expires and the pod looks unschedulable
again.

**Mitigation.** Set `limits` on every NodePool so the blast radius is bounded, and consider
raising `--batch-max-duration`. Reducing node boot time helps most.

## Servers exist in Hetzner with no NodeClaim

The provider garbage-collects these, but not immediately and not unconditionally:

- Only servers carrying this cluster's `karpenter.sh/managed-by` label. Anything else, including
  every Terraform-created control plane node, is invisible to it.
- Only after five minutes, because a server exists before its NodeClaim records a provider ID
  and reaping sooner would delete nodes mid-creation.
- Never when there are **no** NodeClaims at all, which is indistinguishable from having lost
  them rather than from every server being an orphan.

`karpenter_hcloud_orphaned_servers_reaped_total` should be flat at zero. A non-zero rate means
creates are succeeding at Hetzner and failing to be recorded.

## Useful metrics

```
karpenter_hcloud_offering_unavailable          # pairs suppressed after a real capacity failure
karpenter_hcloud_offering_published_available  # what Hetzner CLAIMS about the same pairs
karpenter_hcloud_launch_failures_total         # by server type, location and error class
karpenter_hcloud_launch_duration_seconds       # includes fall-through, so the slow case is visible
karpenter_hcloud_catalog_stale                 # 1 when the last refresh failed
```

The two `offering_*` series are deliberately separate. Hetzner's published availability flag is
neither sufficient nor necessary: types reported unavailable have been ordered successfully, and
types reported available have returned `resource_unavailable`. Nothing in this provider gates on
it. Graphing both makes the divergence visible, which is the only honest use for it.

On a two-replica deployment the standby publishes zeros for the catalog gauges, because those
loops only run on the elected leader. Filter to the leader or use `max()`.
