# Node repair

Karpenter core can reboot or replace a node whose conditions say it is unhealthy. It is off
unless the `NodeRepair` feature gate is on:

```yaml
controller:
  featureGates:
    NodeRepair: true
```

## Policies

| Node condition | After | Action |
|---|---|---|
| `Ready=Unknown`: the kubelet has stopped reporting | 10 minutes | Reboot in place |
| `Ready=Unknown` | 30 minutes | Replace |
| `Ready=False`: the kubelet reports NotReady | 30 minutes | Replace |

Times count from the condition's last transition. When both `Ready=Unknown` policies are due,
replace wins, so the 30-minute replace only applies when no reboot ran first, for example because
a disruption budget held it back.

None of them drain the node. By ten minutes the `not-ready` and `unreachable` NoExecute taints
have already evicted every pod without a longer toleration (the default is 300 seconds), and a
drain against a kubelet that is not answering cannot finish.

Only `Ready=Unknown` is rebooted. The provider is not told which condition asked for the reboot,
so it cannot choose a graceful ACPI reboot for a node whose OS is still running, and a hard reset
is only the right tool when the OS is unreachable.

## What a reboot does

1. Karpenter taints the node `karpenter.sh/rebooting:NoSchedule`.
2. The provider checks that the server carries this cluster's `karpenter.sh/managed-by` label
   and the NodeClaim's name in `karpenter.itsh.dev/nodeclaim`. It also refuses a server that is
   off, or whose last 50 actions include a stop, shutdown or rescue, since the provider issues
   none of those and someone may be holding the node down. It then resets the server through
   the Hetzner API, which is a hard power cycle.
3. Karpenter removes the taint once the node reports a new boot ID, and allows 20 minutes for
   that and `Ready=True`.

The node rejoins with the kubelet credentials it already has; cloud-init does not run again.

If the node does not come back in time, or the provider rejects the reboot, the node is replaced.
Once two reboots of a node have been committed within 24 hours, the next repair replaces it. That
history lives in the controller process, so a restart or a leader change clears it.

## What holds repair back

- **The unhealthy-node breaker.** Repair stops in a NodePool while more of its nodes are unhealthy
  than 20% of its size, rounded up. That is one node for pools of up to five, so the breaker
  never trips in a one-node pool.
- **Disruption budgets.** Repair counts as the `Unhealthy` reason. A budget with `nodes: "0"` and
  no `reasons` blocks it along with everything else; list `Empty`, `Drifted` and `Underutilized`
  to freeze only voluntary disruption. Budgets do not apply to a node that has not finished
  initializing.
- **`karpenter.sh/do-not-repair: "true"`** on the Node blocks repair for it.
  `karpenter.sh/do-not-disrupt` does not. Set it before stopping a node or booting it into
  rescue: a refused reboot replaces the node, which deletes its server unless the server has
  Hetzner's `delete_protection`.

## Observing it

| Metric | What it counts |
|---|---|
| `karpenter_nodes_reboots_total{result}` | Reboots by outcome: `succeeded`, `provider_error`, `recovery_timeout`, `invalid_request` |
| `karpenter_nodes_reboot_duration_seconds` | Request to outcome |
| `karpenter_nodes_reboot_recovery_duration_seconds` | Reboot issued to node back, successful reboots only |
| `karpenter_voluntary_disruption_decisions_by_nodepool_total{reason="unhealthy"}` | Repair decisions, `decision="reboot"` among them |

Core creates each series on its first event, on the leader only, so an alert on `increase()`
alone misses the first event after every controller restart or leader change. Count a series that
appeared inside the window as well:

```promql
(increase(karpenter_nodes_reboots_total[1h]) > 0)
  or (karpenter_nodes_reboots_total unless karpenter_nodes_reboots_total offset 1h)
```

A successful reboot ends the NodeClaim's `Rebooting` condition `False` with reason
`RebootSucceeded`. A failed one deletes the NodeClaim, and its `RebootFailed` event names the
cause. A `NodeRepairBlocked` event means the breaker held a candidate back.
