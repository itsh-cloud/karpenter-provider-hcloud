# Acceptance tests

These run against a live cluster and a live Hetzner project. They are not `go
test`: what they check is whether the provider does the right thing when
Hetzner says no, which cannot be faked convincingly.

## 7a, capacity fall-through

Always runnable, no teardown required beyond deleting what it creates.

```bash
kubectl apply -f test/acceptance/7a-capacity-fallthrough.yaml
```

Watch:

```bash
kubectl get nodeclaims -l itsh.dev/acceptance=7a -w
kubectl logs -n karpenter -l app.kubernetes.io/name=karpenter-provider-hcloud -f \
  | grep -E "falling through|capacity|created nodeclaim|disrupt"
```

What you should see depends on Hetzner's stock, and both outcomes are a pass:

- **cx33 available in nbg1.** One cx33 appears. Nothing else happens. The
  interesting assertion is a negative one: the provider did NOT pick cpx32,
  which is four times the price, and did not thrash.
- **cx33 out of stock in nbg1.** The create fails with a capacity-class code,
  the log shows `falling through to the next candidate`, the pair is marked
  unavailable, and a cpx32 appears instead. The pod is scheduled either way.

The part worth waiting for is what happens **after** stock returns. The
suppression expires after five minutes, cx33 becomes a candidate again, and
consolidation should simulate replacing the cpx32, find the replacement cheaper
with the pod still schedulable, and execute a replace. That is the behaviour
cluster-autoscaler could not perform at all: it could delete an underutilised
node, never replace a correctly-utilised but wrongly-typed one. It is the reason
this project exists.

Useful signals:

```bash
# Which pairs are currently suppressed, and why.
karpenter_hcloud_offering_unavailable

# What Hetzner PUBLISHES about the same pairs. Divergence between the two is
# expected and interesting: the published flag is neither sufficient nor
# necessary, which is why nothing gates on it.
karpenter_hcloud_offering_published_available

karpenter_hcloud_launch_failures_total{reason="capacity"}
```

Teardown:

```bash
kubectl delete -f test/acceptance/7a-capacity-fallthrough.yaml
```

## 7b, cross-location ordering

**Stock-gated and optional.** Only run it if a fresh API check shows a CX type
orderable in hel1, and it needs explicit teardown.

It asserts the decision cluster-autoscaler's static list got wrong: a cx33 in
hel1 should be preferred over a cpx32 in nbg1, because price ranks it first and
region is not a tiebreak.

Running it requires temporarily setting the shared NodeClass's
`spec.locations` to **`[nbg1, hel1]`**, keeping every production location in the
list. Write the full list; do not "add hel1" to something you have not read.

The protection is that nbg1 stays IN the list, and nothing else. An earlier
version of this note claimed the safety came from `spec.locations` being in the
not-hashed set, which is a non-sequitur: it carries `hash:"ignore"` precisely
BECAUSE drift compares it live instead. If `spec.locations` is currently unset,
writing `[hel1]` alone narrows the resolved set to hel1 and drifts every nbg1
node, with the NodeClass reporting Ready throughout.

Removing hel1 at teardown drifts only the hel1 node, which is the teardown
behaviour you want.

Do **not** reach for a second NodeClass to avoid the edit. There is exactly one,
deliberately, and a second would quietly undo that.
