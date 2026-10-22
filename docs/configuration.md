---
title: "Configuration"
weight: 90
description: "Reference runtime JSONC settings and the Helm values that render and deploy them."
---

# Configuration

The binary reads strict JSONC with `sourcegateway --config=<path>`; unknown fields are rejected.

The Helm chart renders `.Values.config` into `/etc/sourcegateway/config.jsonc`, adds TLS paths, and removes deployment-only `secretProviderClass` and `secretName` keys.

Plaintext upstreams require a separate, exact `--allow-insecure-upstream=<origin>` development flag. Repeat the flag for multiple origins; the chart passes flags from `extraArgs`.

## Runtime JSONC

| Field | Default or requirement |
| --- | --- |
| `tokenAudience` | `sourcegateway.dev` |
| `objectCacheCapacity` | Required positive Kubernetes quantity; chart default `20Gi`. |
| `maxObjectSize` | Positive Kubernetes quantity limiting puts, copies, and completed multipart objects; default `15Mi`. Set `null` for no gateway limit. |
| `credentialsDirectory` | `/var/run/sourcegateway/secrets`, absolute. |
| `githubUserAnnotation` | `sourcegateway.dev/github-user`. |
| `listenAddress` | `:8443` |
| `metricsAddress` | `:9443` |
| `storageDirectory` | `/var/lib/sourcegateway`, absolute. |
| `tlsCertificateFile`, `tlsPrivateKeyFile` | Required serving TLS paths. |
| `credentials` | Credential names grouped under `gitRepository` and `objectStorageBucket`. |

### Credentials

Each credential contains `kind`, a nonempty `namespaces` allowlist, and an optional trusted `origin`. `"*"` explicitly permits every resource namespace. Names are globally unique, and `encryption-key` is reserved.

Git credentials default to `https://github.com`; set `origin` for GitHub Enterprise Server. The selected repository URL must have exactly that origin. Valid Git kinds are `githubApp` and `githubUser`.

GCS credentials default to `https://storage.googleapis.com`. AWS credentials default to the regional S3 dual-stack origin resolved by the AWS SDK. Set `origin` for a custom S3 service. A resource may omit `spec.backend.endpoint` or provide a base URL, including a path, on that effective origin. Valid kinds are `awsAccessKey`, `awsDefault`, `gcsDefault`, and `gcsServiceAccount`.

Credential files and formats are listed in [Deployment](deployment.md).

### Example Config JSONC file

```jsonc
{
  "tokenAudience": "sourcegateway.dev",
  "objectCacheCapacity": "20Gi",
  "maxObjectSize": "15Mi",
  "credentialsDirectory": "/var/run/sourcegateway/secrets",
  "credentials": {
    "gitRepository": {
      "example-github": {"kind": "githubApp", "namespaces": ["agents"]}
    },
    "objectStorageBucket": {
      "example-aws": {"kind": "awsAccessKey", "namespaces": ["agents"]}
    }
  }
}
```

## Chart-only values

These control Kubernetes resources and are not runtime JSONC: top-level `enabled`, `crds`, `extraArgs`, `image`, `imagePullSecrets`, naming overrides, `serviceAccount`, `service`, encryption-key and TLS secret sources, `servingTLS.projected`, `persistence`, resources, Pod/container security contexts, NetworkPolicy, scheduling, and each credential's `secretProviderClass` or `secretName`.

Persistence is enabled by default; disabling it uses `emptyDir` storage. The chart does not create an operational Service; use a `PodMonitor` or create a Service when a collector needs one, and restrict `networkPolicy.metricsIngressFrom`. `hostUsers` defaults to `false` with a UID 0 security context for direct access to mounted private files; `hostUsers: true` requires a non-root `podSecurityContext` with an `fsGroup`.

See the chart's complete [values.yaml](https://github.com/sourcegateway/sourcegateway/blob/main/deploy/helm/values.yaml). Config or secret-mount changes alter Pod checksum annotations and trigger replacement.
