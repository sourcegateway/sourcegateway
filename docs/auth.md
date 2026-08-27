---
title: "Auth & Access"
weight: 50
description: "Authenticate ServiceAccounts with projected tokens and authorize requests with CEL policies."
---

# Authentication and Access Control

Source Gateway authenticates Kubernetes ServiceAccounts and applies per-source CEL policies. These policies are not Kubernetes RBAC and need not be role-only: they can consider ServiceAccount identity and operation-specific request fields.

## Project a token

Add a dedicated token to the calling Pod rather than using its Kubernetes API token:

```yaml
volumes:
  - name: sourcegateway-token
    projected:
      sources:
        - serviceAccountToken:
            path: token
            audience: sourcegateway.dev
            expirationSeconds: 3600
```

Mount the volume read-only and send its rotating token as `Authorization: Bearer <token>`. The configured audience defaults to `sourcegateway.dev`.

Source Gateway sends the token to the Kubernetes `authentication.k8s.io/v1` TokenReview API with that exact audience. Authorization receives only the verified `sa.namespace`, `sa.name`, and `sa.uid`; JWT claims and caller-supplied identity headers are not trusted. Invalid credentials return `401`, policy denials return `403`, and a TokenReview outage returns `503`.

[Nono](https://nono.sh/) is optional. Its trusted supervisor can hold the projected token and proxy requests for a sandboxed tool, keeping the real token and direct network access outside that tool. Source Gateway still authenticates the same ServiceAccount identity.

## CEL policies

Rules are additive: any applicable expression returning `true` allows the operation. No match, invalid policy, or evaluation failure denies it.

```yaml
authorization:
  rules:
    - name: team-readers
      operations: [git.read]
      allow: sa.namespace == "agents"
    - name: own-object-prefix
      operations: [object.list, object.read, object.write, object.delete]
      allow: request.key.startsWith("workspaces/" + sa.name + "/")
    - name: constrained-push
      operations: [git.push]
      allow: >-
        sa.namespace == "agents" &&
        request.branch.startsWith("workspace/" + sa.name + "/") &&
        !request.force && !request.delete
```

Git rules accept `git.read` and `git.push`.

Object Storage rules accept `bucket.list`, `bucket.head`, `object.list`, `object.read`, `object.write`, and `object.delete`.

Request fields are `operation`, `branch`, `create`, `delete`, `force`, `bucket`, `key`, and `size` under `request`.

Each source allows at most 32 named rules; expressions are limited to 4 KiB and bounded CEL complexity/cost. An invalid policy makes that source unavailable. Treat permission to edit source resources as security-sensitive because it changes both upstream selection and access policy.
