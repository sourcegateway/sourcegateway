---
title: "Concepts"
weight: 20
description: "Understand sources, upstreams, local data, workload identity, and request flow."
---

# Concepts

**Source** is a namespaced `GitRepository` or `ObjectStorageBucket`. It binds one upstream target, one operator-configured credential name, local cache settings, and additive CEL authorization rules.

**Upstream** is the authoritative Git repository or S3/GCS bucket. Source Gateway never turns local data into an independent system of record. An acknowledged push or object write has completed synchronously upstream; there is no write-back queue or offline write mode.

**Mirror and cache** are reconstructable data in gateway storage. Git repositories use complete mirrors refreshed on a schedule. Object reads populate an LRU cache; lists still query the upstream. Authorization always happens before local data is served.

**Workload identity** is the namespace, name, and UID of a Kubernetes ServiceAccount verified by TokenReview. A projected token's audience scopes that credential to the gateway.

## Request flow

1. The workload sends a bearer token with a Git or path-style object request.
2. Source Gateway verifies the token through Kubernetes TokenReview.
3. It finds the source and evaluates the operation's CEL rules.
4. An allowed read uses fresh local data when possible, otherwise the upstream.
5. A write is validated, committed upstream, and only then acknowledged and reflected locally.

The initial architecture is one Source Gateway replica running the Kubernetes controllers and Git/object storage proxy. The chart uses one PVC by default; ephemeral storage is optional. Healthy sources continue serving if another source is unready.
