#!/usr/bin/env python3
"""Render config/crd into the Helm chart's templates.

A plain `cp` cannot carry the three things the chart needs:

  * an install-time toggle, so a GitOps setup that applies CRDs separately can
    turn them off;
  * helm.sh/resource-policy: keep, so `helm uninstall` leaves the CRDs, and
    therefore every HCloudNodeClass, in place. Without it, removing the release
    deletes the record of how every running node was built.
  * argocd.argoproj.io/sync-options: Prune=false,Delete=false, which is the
    same protection against the controller most people deploy this with.
    Deleting the NodePool or NodeClaim CRD cascades to every CR of that kind,
    and deleting a NodePool CR drains and deletes every node it owns, so an
    ordinary prune takes out the whole fleet. Whether Argo honours
    helm.sh/resource-policy as an implicit prune block is not documented, so
    this is stated rather than assumed.

Idempotent: the destination is rebuilt from config/crd each run.
"""

import pathlib
import sys

GUARD_OPEN = "{{- if .Values.crds.enabled }}"
GUARD_CLOSE = "{{- end }}"
KEEP = (
    "    helm.sh/resource-policy: keep\n"
    "    argocd.argoproj.io/sync-options: Prune=false,Delete=false\n"
)
ANNOTATIONS = "  annotations:\n"


def stamp(src: pathlib.Path, dst: pathlib.Path) -> None:
    doc = src.read_text()

    # Counting occurrences is not enough: karpenter.sh_nodepools.yaml contains a
    # second "  annotations:" deep inside the schema, describing the annotations
    # a NodePool puts on the nodes it creates. Stamping that one would edit the
    # CRD's schema rather than the CRD.
    #
    # The top-level metadata block is everything before the first column-zero
    # "spec:", so the annotations key we want is the first occurrence that falls
    # inside it. Verified rather than assumed, because getting this wrong is
    # silent in both directions.
    body_start = doc.find("\nspec:\n")
    if body_start == -1:
        raise SystemExit(f"{src}: no top-level spec:, this does not look like a CRD")

    at = doc.find(ANNOTATIONS)
    if at == -1 or at > body_start:
        raise SystemExit(
            f"{src}: no metadata.annotations block before spec:; controller-gen "
            "always emits one, so the stamping assumption no longer holds"
        )

    doc = doc[:at] + ANNOTATIONS + KEEP + doc[at + len(ANNOTATIONS):]
    dst.write_text(f"{GUARD_OPEN}\n{doc.rstrip(chr(10))}\n{GUARD_CLOSE}\n")


def main() -> None:
    src_dir = pathlib.Path("config/crd")
    dst_dir = pathlib.Path("chart/karpenter-provider-hcloud/templates/crds")
    dst_dir.mkdir(parents=True, exist_ok=True)

    for stale in dst_dir.glob("*.yaml"):
        stale.unlink()

    sources = sorted(src_dir.glob("*.yaml"))
    if not sources:
        raise SystemExit(f"{src_dir}: no CRDs to stamp; run controller-gen first")
    for src in sources:
        stamp(src, dst_dir / src.name)

    print(f"stamped {len(sources)} CRDs into {dst_dir}", file=sys.stderr)


if __name__ == "__main__":
    main()
