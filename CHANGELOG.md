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
