---
title: "Development"
weight: 100
description: "Set up the offline localhost stack, run checks, regenerate APIs, and build images."
---

# Development

Install [mise](https://mise.jdx.dev/), Git, OpenSSL, and tmux, then run `make setup` once while network access is available. Mise installs the pinned Go, Helm, kubeconform, rclone, Overmind, and SeaweedFS versions; Go installs the pinned envtest binaries. Vendored kubeconform fixtures live under `tests/kubeconform`. After setup, the development stack is localhost-only and requires neither Docker nor external hosts.

Source Gateway targets Kubernetes 1.37. Upstream envtest assets currently end
at 1.36.2, so controller end-to-end tests use that latest published version.
Set `ENVTEST_VERSION` when a newer installed envtest version is available.

```console
make dev
```

The single [Procfile](https://github.com/sourcegateway/sourcegateway/blob/main/Procfile.dev) runs an envtest API server/etcd with the CRDs, SeaweedFS as an S3 backend, a local HTTPS Git server, and Source Gateway. State defaults to `tmp/dev`; the stack binds its services to loopback and explicitly permits SeaweedFS's HTTP origin. Use `make e2e` for an isolated offline stack and end-to-end Git, object, policy, metrics, and recovery checks.

## Checks and generation

```console
make precommit   # formatting, generated-file synchronization, go vet
make lint        # golangci-lint
make test        # race-enabled unit tests
make helm        # chart lint, render, and strict Kubernetes schema validation
```

Run `make generate` after changing API types or RBAC markers. It regenerates deepcopy code, CRDs, RBAC, and conditional chart CRD templates; do not edit generated artifacts manually. `make validate-generated` checks that generated copies are present and synchronized.

## Builds

`make build` creates `bin/sourcegateway`. Production images are packaging-only builds made with ocimage.

## Releases

Push release commits to `main`, then tag them as `vX.Y.Z`. The release workflow
requires successful push CI on `main` for that exact commit. Tags pushed while
CI is queued or running wait up to 30 minutes; failed, cancelled or timed-out CI
blocks publication. A successful CI run for another commit or a pull request
does not qualify. After retrying failed CI successfully, rerun the release.
