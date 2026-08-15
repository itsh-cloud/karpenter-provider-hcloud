image := env("IMAGE", "ghcr.io/itsh-cloud/karpenter-provider-hcloud")
version := `git describe --tags --always --dirty 2>/dev/null || echo "dev"`

controller_gen_version := "v0.20.1"

default:
    @just --list

# Build the controller binary
build:
    go build -buildmode=pie -trimpath -ldflags="-s -w -X main.version={{ version }}" -o bin/karpenter-provider-hcloud ./cmd/karpenter-provider-hcloud

# Build Docker image
docker-build:
    docker build --build-arg VERSION={{ version }} -t {{ image }}:{{ version }} -t {{ image }}:latest .

# Push Docker image
docker-push:
    docker push {{ image }}:{{ version }}
    docker push {{ image }}:latest

# Run tests
test:
    go test ./...

# Run tests with race detector and coverage (what CI runs)
test-ci:
    go test -race -coverprofile=coverage.out ./...

# Run locally (requires kubeconfig and HCLOUD_TOKEN)
run *ARGS:
    go run ./cmd/karpenter-provider-hcloud {{ ARGS }}

# Download and tidy dependencies
deps:
    go mod download
    go mod tidy

# Run linter
lint:
    golangci-lint run ./...

# Run go vet
vet:
    go vet ./...

# Regenerate deepcopy + CRDs, and vendor the upstream karpenter.sh CRDs.
# The karpenter.sh CRDs are copied from the PINNED module version so they can
# never drift from the schema the binary is compiled against.
generate:
    controller-gen object paths="./api/..." output:object:dir=api/v1alpha1
    controller-gen crd paths="./api/..." output:crd:dir=config/crd
    just _vendor-karpenter-crds
    cp config/crd/*.yaml chart/karpenter-provider-hcloud/templates/crds/

# Same, but via a container so controller-gen need not be installed locally
generate-docker:
    docker run --rm -v "$PWD":/work -w /work golang:1 sh -c "\
      go install sigs.k8s.io/controller-tools/cmd/controller-gen@{{ controller_gen_version }} && \
      controller-gen object paths=./api/... output:object:dir=api/v1alpha1 && \
      controller-gen crd paths=./api/... output:crd:dir=config/crd"
    just _vendor-karpenter-crds
    cp config/crd/*.yaml chart/karpenter-provider-hcloud/templates/crds/

_vendor-karpenter-crds:
    #!/usr/bin/env bash
    set -euo pipefail
    ver=$(go list -m -f '{{{{.Version}}}}' sigs.k8s.io/karpenter)
    src="$(go env GOMODCACHE)/sigs.k8s.io/karpenter@${ver}/pkg/apis/crds"
    echo "vendoring karpenter.sh CRDs from ${src}"
    cp "${src}/karpenter.sh_nodepools.yaml" "${src}/karpenter.sh_nodeclaims.yaml" config/crd/

# Print the full price-ordered offering table (read-only, needs HCLOUD_TOKEN)
catalog-dump *ARGS:
    go run ./cmd/karpenter-provider-hcloud catalog-dump {{ ARGS }}

# Print predicted capacity/allocatable per instance type
instance-types *ARGS:
    go run ./cmd/karpenter-provider-hcloud instance-types {{ ARGS }}

# Render the cloud-init a NodeClaim would receive, for diffing
render-userdata *ARGS:
    go run ./cmd/karpenter-provider-hcloud render-userdata {{ ARGS }}

# End-to-end tests. Needs a SCRATCH hcloud project, creates and destroys real
# servers, and costs money per run. Never wire this into CI.
e2e:
    go test -tags e2e -v -timeout 30m ./test/e2e/...
