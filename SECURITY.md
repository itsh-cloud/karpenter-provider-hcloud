# Security

## Reporting a vulnerability

Please report security issues privately via GitHub's ["Report a vulnerability"][advisories]
form on this repository rather than opening a public issue.

[advisories]: https://github.com/itsh-cloud/karpenter-provider-hcloud/security/advisories/new

## Threat model

Operators should understand three properties of this controller before deploying it. None of
them are defects; they are inherent to what a node-lifecycle controller does, and they are
documented here so the blast radius is a decision rather than a surprise.

### The Hetzner API token is the real blast radius

Hetzner Cloud API tokens are **project-scoped** and are either read-only or read-write, with no
per-resource scoping. This controller needs read-write to create and delete servers.

Code execution inside the controller pod therefore yields full control of the entire Hetzner
project: every server can be deleted, including control-plane nodes this controller does not
manage; volumes can be deleted; firewalls can be rewritten.

The in-process guards this controller implements (refusing to delete servers that do not carry
its `karpenter.sh/managed-by` label, NodePool limits, disruption budgets) constrain **bugs and
accidental operations**. They do not constrain a compromise, because they live inside the
binary that would be compromised. Likewise, Terraform `prevent_destroy` on a server is a
plan-time lifecycle guard and has no effect against a direct API call.

Recommendations:

- Give this controller a **dedicated API token**, not one shared with the CCM or CSI driver, so
  it can be revoked without a collateral rotation of everything else.
- Restrict the namespace's egress to the Kubernetes API server, the Hetzner API and DNS.
- Consider Hetzner's server-level `delete_protection` on nodes this controller must never
  touch. It is the only guard that lives outside the binary, though a read-write token can
  disable it with one additional call.
- Treat token revocation as the incident response.

### Creating bootstrap tokens is a privilege-escalation primitive

To join nodes, the controller creates `bootstrap.kubernetes.io/token` secrets in `kube-system`.
Anyone able to do that can obtain a `system:node:<name>` client certificate through the normal
CSR auto-approval flow.

This is inherent to the join function. It is bounded in two ways: Kubernetes enforces that
`auth-extra-groups` begins with `system:bootstrappers:`, so `system:masters` cannot be
injected; and this controller requests **no read verbs on secrets** — no `list`, no `watch`,
and `get` only by explicit resource name. The absence of general secret read is deliberate and
load-bearing: it blocks the common escalation of minting a service-account-token secret for a
privileged service account and reading the result.

Tokens are minted per NodeClaim with a short TTL and an `ownerReference` on the NodeClaim, so
they are garbage-collected when the NodeClaim is deleted for any reason, including a failed
registration.

### The join token is briefly readable on the node

A node's cloud-init `user_data` contains a live bootstrap token, readable at
`/var/lib/cloud/instance/user-data.txt` on the node itself and through the Hetzner console.
The short TTL bounds this in time. Do not extend the TTL beyond what your node registration
actually requires.

## Supported versions

Pre-release. Only the latest tag receives fixes.
