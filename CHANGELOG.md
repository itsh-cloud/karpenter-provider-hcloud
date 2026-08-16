# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Repository skeleton: build tooling, linting, CI, and a bare operator binary that starts
  `sigs.k8s.io/karpenter`'s manager with no controllers registered.
- Pinned `sigs.k8s.io/karpenter` v1.14.0. The pin is exact, and upstream minor bumps are
  reviewed by hand rather than auto-merged, because they have historically changed
  provider-facing interfaces.
- `HCloudNodeClass` status controller: resolves every selector against the Hetzner API,
  publishes the resolved identifiers and the usable locations, and rolls the per-resource
  conditions up into `Ready`, which is what Karpenter gates provisioning on. A missing
  resource is reported as a `False` condition naming the selector; a failed API call is
  retried and leaves the condition alone, so a blip cannot take a healthy class down.
- `HCloudNodeClass` hash controller, including the hash-version back-fill that keeps a
  change to the hash generator from marking the whole fleet drifted at once.
- Termination finalizer holding an `HCloudNodeClass` open while NodeClaims still reference
  it. Karpenter core neither blocks nor cascades here, so without it a delete leaves nodes
  running against a class that no longer exists.
- `HCloudNodeClass` and `HCloudNodeClassList` are registered into client-go's global scheme.
  `operatorpkg`'s GVK lookup resolves through that scheme and panics on an unknown type, so
  the omission would have killed the binary at startup rather than failing a reconcile.
