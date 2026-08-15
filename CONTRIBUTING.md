# Contributing

Contributions are welcome. This project is early, so opening an issue before a large change
will save you time.

## Development

Requires Go (version in `go.mod`) and [`just`](https://github.com/casey/just).

```bash
just deps      # go mod download && tidy
just build
just test
just lint
just vet
```

CI runs `golangci-lint`, `go vet`, `go test -race`, `govulncheck` and `helm lint`. Running
`just lint && just vet && just test` locally covers most of it.

### Regenerating CRDs

`api/v1alpha1` is the source of truth for the `HCloudNodeClass` CRD.

```bash
just generate          # needs controller-gen installed
just generate-docker   # same, in a container
```

This also re-vendors the upstream `karpenter.sh` CRDs from the **pinned** `sigs.k8s.io/karpenter`
version, so they can never drift from the schema the binary is compiled against. Commit the
regenerated files.

### Tests

Plain `testing`, table-driven, with `sigs.k8s.io/controller-runtime/pkg/client/fake` for
Kubernetes interactions and an in-memory fake for the Hetzner API. No ginkgo, no testify.

Anything that talks to the real Hetzner API belongs behind the `e2e` build tag. `just e2e`
creates and destroys real servers against a scratch project and costs money, so it is never
wired into CI.

## Commit and PR conventions

- [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `chore:`,
  `docs:`, `refactor:`, `test:`, `ci:`, `perf:`.
- Keep the subject line short. Add a body only when it explains something the diff does not.
- One logical change per PR.

## Upstream compatibility

This provider implements the `CloudProvider` interface from `sigs.k8s.io/karpenter`, which is
pinned to an exact version. Upstream **minor** releases have historically changed
provider-facing interfaces, so those bumps are reviewed by hand rather than auto-merged. If a
change requires a newer core, say so explicitly in the PR.

## License

By contributing you agree that your contributions are licensed under Apache 2.0.
