---
title: "CRD Reference"
weight: 80
description: "Reference the v1alpha1 GitRepository and ObjectStorageBucket fields and status."
---

# CRD Reference

Both namespaced resources use `apiVersion: sourcegateway.dev/v1alpha1` and share `spec.authorization.rules`. A rule has a unique nonempty `name`, one or more supported `operations`, and a nonempty CEL `allow` expression (maximum 4 KiB); there are at most 32 rules.

## GitRepository

| Field | Meaning |
| --- | --- |
| `spec.upstream.url` | HTTPS GitHub.com or GitHub Enterprise repository URL. |
| `spec.upstream.credential` | Name in runtime `credentials.gitRepository`, permitted for this namespace. |
| `spec.mirror.refreshInterval` | Positive periodic mirror refresh interval. |
| `spec.mirror.maxStaleness` | Positive maximum age that may serve reads; gateway pushes update the mirror immediately. |
| `spec.authorization.rules` | Rules using `git.read` and/or `git.push`. |

`status.path` is `/git/<namespace>/<name>.git`.

## ObjectStorageBucket

| Field | Meaning |
| --- | --- |
| `spec.backend.provider` | `s3` or `gcs`. |
| `spec.backend.endpoint` | Optional object storage API base URL. Its origin must match the selected credential's effective origin. HTTPS is required unless the process permits that exact HTTP origin. |
| `spec.backend.region` | Required for S3 and omitted for GCS. |
| `spec.backend.bucket` | Authoritative provider bucket name. |
| `spec.backend.credential` | Name in runtime `credentials.objectStorageBucket`, permitted for this namespace. |
| `spec.cache.capacity` | Positive per-source cache maximum, no greater than the global maximum. |
| `spec.cache.ttl` | Positive retention duration for cached reads; gateway writes refresh or evict affected entries. |
| `spec.authorization.rules` | Rules using the `bucket.*` and `object.*` operations. |

`status.bucketName` is the stable S3-compatible name clients use at the gateway; do not derive or replace it with the upstream bucket name.

## Shared status

`status.observedGeneration` identifies the reconciled generation, `status.policyRevision` identifies the compiled policy, and `status.conditions` contains `PolicyReady`, `CredentialsReady`, `UpstreamReady`, and `Ready`. A source is served only when `Ready=True` for its current generation.
