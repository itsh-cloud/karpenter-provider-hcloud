FROM golang:1-alpine AS builder
RUN apk add --no-cache build-base
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN go build -buildmode=pie -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o karpenter-provider-hcloud ./cmd/karpenter-provider-hcloud

FROM alpine:3
RUN apk add --no-cache ca-certificates
WORKDIR /
COPY --from=builder /app/karpenter-provider-hcloud .
USER 65532:65532
ENTRYPOINT ["/karpenter-provider-hcloud"]
