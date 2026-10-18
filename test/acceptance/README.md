# Acceptance tests

These run against a live cluster and a live Hetzner project, and they create real servers.
They are not `go test`: what they check is whether the provider does the right thing when
Hetzner says no, which cannot be faked convincingly.

Each NodePool below pins `karpenter.sh/capacity-type: [on-demand]`, as every NodePool on
Hetzner must. Left unconstrained, the requirement is unbounded, so consolidation pins every
replacement to spot. The launch fails, and core retries it about every 27 seconds
indefinitely, creating and deleting a NodeClaim each time.

The manifests reference an `HCloudNodeClass` named `default`. Point `nodeClassRef` at yours.

## Capacity fall-through

Always runnable, no teardown required beyond deleting what it creates.

```bash
kubectl apply -f test/acceptance/capacity-fallthrough.yaml
```

Watch:

```bash
kubectl get nodeclaims -l karpenter.itsh.dev/acceptance=capacity-fallthrough -w
kubectl logs -n karpenter -l app.kubernetes.io/name=karpenter-provider-hcloud -f \
  | grep -E "falling through|capacity|created nodeclaim|disrupt"
```

What you should see depends on Hetzner's stock, and both outcomes are a pass:

- **cx33 available in nbg1.** One cx33 appears. Nothing else happens. The interesting
  assertion is a negative one: the provider did NOT pick cpx32, which costs several times
  as much, and did not thrash.
- **cx33 out of stock in nbg1.** The create fails with a capacity-class code, the log shows
  `falling through to the next candidate`, the pair is marked unavailable, and a cpx32
  appears instead. The pod is scheduled either way.

The part worth waiting for is what happens **after** stock returns. The suppression expires
after five minutes, cx33 becomes a candidate again, and consolidation should simulate
replacing the cpx32, find the replacement cheaper with the pod still schedulable, and
execute a replace. cluster-autoscaler cannot do this: it can delete an underutilised node,
never replace a correctly-utilised but wrongly-typed one.

Useful signals:

```bash
# Which pairs are currently suppressed, and why.
karpenter_hcloud_offering_unavailable

# What Hetzner PUBLISHES about the same pairs. Divergence between the two is
# expected: the published flag is neither sufficient nor necessary, which is
# why nothing gates on it.
karpenter_hcloud_offering_published_available

karpenter_hcloud_launch_failures_total{reason="capacity"}
```

Teardown:

```bash
kubectl delete -f test/acceptance/capacity-fallthrough.yaml
```

## Cross-location ordering

**Stock-gated and optional.** It needs two locations that both offer a cheap type and a
pricier one, a fresh API check that the cheap type is orderable in the second location, and
explicit teardown.

It asserts that price ranks candidates across locations: a cx33 in the second location should
be preferred over a cpx32 in the first, because region is not a tiebreak.

1. Read the NodeClass's current `spec.locations`, then write it back with the second
   location **added** to the full list. Writing only the new location narrows the resolved
   set and drifts every node outside it, with the NodeClass reporting Ready throughout.
2. Run the capacity fall-through manifest with its region requirement widened to both
   locations, and check which pair the NodeClaim lands on.
3. At teardown, write the original list back explicitly. Do not clear the field: location
   drift only applies to an explicit `spec.locations`, so clearing it disables location drift
   and a node in the added location is never drifted away. It would stay, and bill, until
   something else removes it.

`spec.locations` is deliberately not part of the spec hash, because drift compares it live
instead: a node in a location missing from the written list drifts at the next evaluation.
