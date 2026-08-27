# Source Gateway

Source Gateway is a pull-through cache and forward proxy for Git and object
storage on Kubernetes. Workloads use ServiceAccount tokens, and Custom Resources
for `GitRepository` or `ObjectStorageBucket` support CEL-based policies for RBAC.
The object storage client uses an S3-compatible API, and can connect upstream to
S3 or GCS. Upstream credentials are mounted as files, preferably from
operator-managed Secrets Store CSI volumes.

## Deployment

The recommended way to deploy Source Gateway is using the Helm chart in
[`deploy/helm`](deploy/helm). It requires Kubernetes 1.37, Linux nodes that
support Pod user namespaces, serving TLS, and an encryption key. The chart can
mount Secrets Store CSI volumes or native Kubernetes Secrets. Persistent storage
is enabled by default and can be replaced with ephemeral storage. See the chart
README and example values for installation and credential configuration.

## Development

Use `make help` for repository commands, `make setup` to install pinned tools and
Git hooks, `make precommit` before commits, and `make test` for the full unit suite. 
Generated API artifacts are owned by the generation target and should not be edited
directly.

## License

Source Gateway is licensed under the Apache License, Version 2.0.
Copyright The Source Gateway Authors.

See the [LICENSE](./LICENSE) file for details.
