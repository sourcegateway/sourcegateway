# Source Gateway agent guide

## Layout

- `api/v1alpha1`: Kubernetes API types and generated deepcopy code.
- `cmd/sourcegateway`: executable entrypoint.
- `internal`: private controller, server, authentication, and provider code.
- `deploy/crds`: generated CRDs; regenerate rather than hand-editing.
- `deploy/helm`: bundled production Helm chart; its conditional CRD templates
  must match `deploy/crds`.
- `images/sourcegateway`: production container definition.

Use the Makefile as the command reference. Run `make precommit` for fast checks,
`make test` for race-enabled tests, and Helm lint/template checks after chart
changes. `make image` cross-compiles amd64 and arm64 binaries and packages them
with ocimage; the Containerfile must remain packaging-only. Do not edit
generated files manually. Preserve strict JSONC decoding, credential redaction,
least-privilege RBAC, and the one-replica/PVC architecture.
Never place credentials in configuration, examples, logs, or test fixtures.
