# Troubleshooting

Failure modes that are hard to diagnose from the outside, in rough order of how often they
bite. Each is *what you see*, then *what it actually is*, because in every case below the
symptom points somewhere other than the cause.

## Nothing consolidates, and NodeClaims churn every ~27 seconds

**Symptom.** A node is created, then a stream of NodeClaims appears and disappears. The node
itself never changes.

**Cause.** The NodePool does not constrain `karpenter.sh/capacity-type`. An unconstrained
requirement is *unbounded*, not empty, so Karpenter's test for "does this permit both spot
and on-demand" passes and consolidation pins the replacement to spot. Karpenter expects that
launch to fail harmlessly; it does not, it retries indefinitely, and Hetzner has no spot
capacity at all.

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
`Can't replace with a cheaper node` even though a cheaper type is permitted and available, or
`Node does not have label "topology.kubernetes.io/zone"`.

**Cause.** Karpenter prices a running node by finding an offering whose zone equals the
node's `topology.kubernetes.io/zone` label. This provider puts that label on every NodeClaim,
derived from the location the same way hcloud-cloud-controller-manager derives it, and core
copies it onto the Node at registration. A missing label stops consolidation for that node
with the event above. A label rewritten to a value no offering carries makes the node price
at **0**, and nothing is ever cheaper than zero.

**How to confirm.** Each node's zone should match its NodeClaim's:

```bash
kubectl get nodeclaims -o custom-columns='NODE:.status.nodeName,ZONE:.metadata.labels.topology\.kubernetes\.io/zone'
kubectl get nodes -L topology.kubernetes.io/zone
```

A node whose label differs has had it rewritten by something else in the cluster. Find what
writes it rather than correcting the node by hand.

## Pods with an existing volume are permanently unschedulable

**Symptom.** New pods schedule fine. Pods that mount an existing hcloud PersistentVolume
never schedule, and the scheduler blames node affinity.

**Cause.** Karpenter injects a bound volume's `nodeAffinity` keys as NodeClaim requirements,
and its compatibility check **denies** a custom label that a NodePool leaves undefined. If
the NodePool template does not define `csi.hetzner.cloud/location`, every such pod is
rejected before any node is considered.

**Fix.** This provider puts that key on every offering, so it works out of the box. If you
override requirements on the NodePool template, do not drop it.

## Nodes join but pods land before Karpenter has labelled them

**Symptom.** Bin-packing decisions look wrong for the first few seconds of a node's life.

**Cause.** The node registered without `karpenter.sh/unregistered:NoExecute`. Karpenter logs
an error and carries on, so the consequence is quiet rather than obvious.

**Fix.** This provider renders that taint into the kubeadm join configuration itself. If you
supply a custom bootstrap, you must render it too.

## A NodeClass sits at `Ready=False` and nothing provisions

Read the conditions rather than the roll-up. Each dependency carries its own reason and
message:

```bash
kubectl get hcloudnodeclass <name> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

- `...CredentialRejected` on several conditions at once means the token, not the selectors.
  The message says so explicitly.
- `...NotFound` names the selector that resolved to nothing.
- `CatalogNotFetched` or `CatalogEmpty` is `Unknown`, not `False`, and is about the Hetzner
  API rather than your configuration.

A transient API failure deliberately leaves conditions **untouched** rather than setting them
`Unknown`, so a healthy class keeps working through a Hetzner blip. A condition stuck at
`Unknown` with a `...Unreachable` reason means the API has never been reachable for that
resource.

## Servers exist in Hetzner with no NodeClaim

The provider garbage-collects these, but not immediately and not unconditionally:

- Only servers carrying this cluster's `karpenter.sh/managed-by` label. Anything else,
  including control plane nodes provisioned outside the provider, is invisible to it.
- Only after five minutes, because a server exists before its NodeClaim records a provider ID
  and reaping sooner would delete nodes mid-creation.
- Never when there are **no** NodeClaims at all, which is indistinguishable from having lost
  them rather than from every server being an orphan.

`karpenter_hcloud_orphaned_servers_reaped_total` should be flat at zero. A non-zero rate
means creates are succeeding at Hetzner and failing to be recorded.

## Useful metrics

```
karpenter_hcloud_offering_unavailable          # pairs suppressed after a real capacity failure
karpenter_hcloud_offering_published_available  # what Hetzner CLAIMS about the same pairs
karpenter_hcloud_launch_failures_total         # by server type, location and error class
karpenter_hcloud_launch_duration_seconds       # includes fall-through, so the slow case is visible
karpenter_hcloud_catalog_stale                 # 1 when the last refresh failed
hcloud_api_requests_total                      # every Hetzner API request, retries included, by code, method and api_endpoint
hcloud_api_request_duration_seconds            # request latency by method
karpenter_cloudprovider_errors_total           # errors returned to Karpenter core, by method, error class and NodePool
karpenter_cloudprovider_duration_seconds       # latency of Karpenter core's calls into the provider, by method
```

`hcloud_api_requests_total{code="429"}` is the rate limit being hit. Hetzner documents a
limit of 3600 requests per hour per project. Other clients using the same project share that
limit and are not counted here.

The two `offering_*` series are deliberately separate. Hetzner's published availability flag
is neither sufficient nor necessary: types reported unavailable have been ordered
successfully, and types reported available have returned `resource_unavailable`. Nothing in
this provider gates on it. Graphing both makes the divergence visible.

On a two-replica deployment the standby publishes zeros for the catalog gauges, because those
loops only run on the elected leader. Filter to the leader or use `max()`.
