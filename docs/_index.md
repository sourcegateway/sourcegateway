---
title: "Source Gateway"
weight: 1
description: "Documentation for deploying and using Source Gateway with Git and object storage."
---

# Source Gateway

Source Gateway is a pull-through cache and forward proxy for Git and object
storage on Kubernetes. 

Workloads use ServiceAccount tokens, and Custom Resources for `GitRepository` or 
`ObjectStorageBucket` support CEL-based policies for RBAC.

The object storage client uses an S3-compatible API, and can connect upstream to
S3 or GCS.

Upstream credentials are mounted as files, preferably from operator-managed Secrets Store CSI volumes.

## Start here

- [Get started](getting-started.md) with the Helm chart and one Git source.
- Learn the [core concepts](concepts.md) and [authentication model](auth.md).

## Set up Source Gateway

- Use the [Git](git.md) or deliberately limited [S3-compatible](object-storage.md) interface.
- Configure [deployment](deployment.md), [runtime settings](configuration.md), and [operations](operations.md).

## Reference

- Consult the [CRD reference](crd-reference.md) or [development guide](development.md).
