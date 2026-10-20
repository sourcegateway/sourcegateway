---
title: "Deployment"
weight: 60
description: "Deploy the one-replica Helm architecture with TLS, cache storage, and mounted credentials."
---

# Deployment

The bundled [Helm chart](https://github.com/sourcegateway/sourcegateway/tree/main/deploy/helm) requires Kubernetes 1.37, Linux nodes supporting Pod user namespaces, a serving certificate, and an encryption key. It deploys exactly one Source Gateway replica with a `Recreate` strategy and, by default, a `ReadWriteOnce` PVC.

CRDs are disabled by default while Source Gateway is enabled. For independent lifecycle management, install the same chart twice: first with `crds.enabled=true` and `enabled=false`, then use the defaults for the gateway release. CRDs are regular Helm templates and are upgraded with their release; never enable them in both releases.

## Credential files

For the encryption key and every file-backed credential, set exactly one source:

- `secretProviderClass` mounts a Secrets Store CSI volume and is preferred. Configure files with mode `0400` and enable automatic rotation.
- `secretName` mounts a native Kubernetes Secret with mode `0400`. When using native Secrets, enable Kubernetes encryption at rest because their contents are stored in etcd.

Serving TLS requires exactly one of those sources or `servingTLS.projected` (a Kubernetes `ProjectedVolumeSource`).

The encryption source must publish `encryption-key` as standard base64 for exactly 32 random bytes. The TLS source must publish `tls.crt` and `tls.key` (names are configurable). Credential sources publish the files below:

| Kind | Required files |
| --- | --- |
| `githubApp` | `app-id`, `installation-id`, `private-key.pem` |
| `githubUser` | `username`, `token` |
| `awsAccessKey` | `access-key-id`, `secret-access-key`; optional `session-token` |
| `gcsServiceAccount` | `service-account.json` |

`awsDefault` uses the AWS SDK default credential provider chain, and `gcsDefault` uses Google Application Default Credentials, so neither kind has credential files. Native Secret and CSI volumes are mounted whole rather than with `subPath`, preserving kubelet and provider rotation. Credentials may declare a trusted `origin`; GitHub, GCS, and regional AWS S3 have safe defaults. A resource cannot send a selected credential to another origin.

The chart uses `hostUsers: false`, so Source Gateway's in-container UID 0 maps to a unique unprivileged host UID. Supported secret mounts expose their private files to that in-container UID without copying or interrupting rotation. This requires Linux 6.3+, containerd 2.0+, runc 1.2+ or crun 1.9+, and idmapped-mount support on every volume filesystem. Do not override the chart's root user settings or schedule it on incompatible nodes.

## TLS, storage, and networking

TLS is required for application and operational endpoints. The Service exposes only application TLS on 443. Health probes reach the Pod's TLS port 9443 directly. Use a `PodMonitor` or create a separate Service for metrics, and restrict `networkPolicy.metricsIngressFrom` to the collector. Restrict `networkPolicy.ingressFrom` for application traffic—an empty list allows all namespaces. Operators own provider egress policy because Kubernetes NetworkPolicy cannot select DNS names.

When persistence is enabled, set `persistence.size` above `config.objectCacheCapacity` to leave room for Git mirrors and cache headroom. Set `persistence.enabled=false` to use node-backed `emptyDir` storage instead. Local data is reconstructable, but ephemeral storage makes every pod replacement rebuild mirrors and caches.

Install using a reviewed copy of the chart's [complete credential example](https://github.com/sourcegateway/sourcegateway/blob/main/deploy/helm/values-example.yaml). See [Configuration](configuration.md) for all chart and runtime settings.
