---
title: "Operations"
weight: 70
description: "Monitor readiness, mirrors, caches, recovery, and common Source Gateway failures."
---

# Operations

The private operational listener serves TLS-protected `/healthz`, `/readyz`, and `/metrics` on Pod port 9443. The chart's Service does not expose it. Health probes connect directly; metrics collectors can use a `PodMonitor` or an operator-managed Service with a restricted `networkPolicy.metricsIngressFrom`. Readiness requires Kubernetes API access, serving TLS, writable storage, and a published routing snapshot; one failed source does not make healthy sources unavailable.

Useful metrics include:

- `sourcegateway_git_mirror_refresh_timestamp_seconds`
- `sourcegateway_object_cache_oldest_age_seconds`
- `sourcegateway_object_cache_entries`
- `sourcegateway_object_cache_bytes`

Git refreshes publish complete mirror state atomically. A failed refresh may keep serving the unchanged mirror until `maxStaleness`; older mirrors return `503`. Successful proxied pushes update the mirror immediately. Object cache entries are admitted only after complete validated transfers, expire by source TTL, and are evicted least-recently-used to enforce per-source/global capacity while retaining filesystem headroom. Writes immediately refresh or invalidate the affected cache entry. These age limits therefore mainly bound writes made directly upstream; when all writes pass through Source Gateway, later reads do not use older local data. Lists bypass the cache.

On startup, Source Gateway recovers complete mirrors left around an interrupted atomic swap and verifies their recorded origin and refresh time. It SHA-256 verifies cached object bytes and removes incomplete or corrupt entries. Local storage loss does not lose authoritative data or replay writes: the gateway rebuilds state from upstream. Deleting a source removes its route and local data, never upstream data.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `401 Unauthorized` | Bearer header, projected token rotation, and audience `sourcegateway.dev`. |
| `403 Forbidden` | Source CEL rules and the requested operation/branch/key. |
| Source `Ready=False` | `PolicyReady`, `CredentialsReady`, and `UpstreamReady` conditions; credential namespace allowlist and upstream reachability. |
| Git `503` | Mirror age versus `maxStaleness`, upstream health, and refresh metric. |
| Object `507` | Temporary staging storage; ordinary cache admission is best-effort. |
| Object `409 OperationOutcomeUnknown` | The upstream may have committed a write after its response was lost; inspect authoritative state before retrying. |
| Pod not ready | Kubernetes API access, CSI mounts, TLS files, encryption key, and writable storage. |

Provider errors and request saturation can also return `503`; retry only operations your client can safely repeat. A Git push can likewise report an unknown outcome when Source Gateway cannot confirm all upstream refs; inspect the upstream before retrying. Credentials, provider endpoints, upstream URLs, branches, object keys, and content are redacted. Metrics intentionally retain logical source namespace/name or derived bucket labels so operators can identify unhealthy resources.
