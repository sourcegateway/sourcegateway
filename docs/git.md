---
title: "Git"
weight: 30
description: "Clone, fetch, and make policy-controlled pushes through the Git smart HTTP endpoint."
---

# Git

Each ready `GitRepository` is served at `/git/<namespace>/<name>.git`. Source Gateway supports Git smart HTTP discovery, `git-upload-pack`, and `git-receive-pack`; it does not provide SSH or dumb HTTP.

Inside a workload with a [projected token](auth.md#project-a-token):

```console
TOKEN=$(cat /var/run/secrets/sourcegateway.dev/token)
URL=https://sourcegateway.sourcegateway.svc/git/agents/application.git

git -c "http.extraHeader=Authorization: Bearer $TOKEN" clone "$URL"
git -c "http.extraHeader=Authorization: Bearer $TOKEN" -C application fetch origin
```

Both discovery and fetch require `git.read`. Reads cover the whole repository; policies do not make individual branches confidential. After a refresh failure, an unchanged mirror may continue serving until `maxStaleness`; an older mirror is unavailable. Successful pushes through Source Gateway update the local mirror before success is reported, so this limit mainly bounds changes made directly upstream.

## Public upstreams

Omit `spec.upstream.credential` when the upstream allows anonymous Git access. No credential configuration or secret mount is needed for that repository:

```yaml
apiVersion: sourcegateway.dev/v1alpha1
kind: GitRepository
metadata:
  name: hello-world
  namespace: agents
spec:
  upstream:
    url: https://github.com/octocat/Hello-World.git
  mirror:
    refreshInterval: 5m
    maxStaleness: 1h
  authorization:
    rules:
      - name: agent-read
        operations: [git.read]
        allow: sa.namespace == "agents" && sa.name == "agent"
```

Anonymous **upstream** access does not permit anonymous **client** access: clients still need a valid ServiceAccount token and a matching policy, even when the mirror is cached. HTTPS remains required unless the operator explicitly permits the exact HTTP origin. URLs cannot embed credentials, query parameters or fragments. Restrict who may edit source resources and the gateway's network egress: an anonymous upstream has no credential-origin allowlist.

For a private upstream, set `spec.upstream.credential` to an operator-configured credential name. Invalid, unavailable or namespace-disallowed named credentials fail closed; they never fall back to anonymous access. Removing a credential does not purge already cached repository data; source policies and mirror freshness still govern reads. Pushes always require `git.push` authorization and acceptance by the upstream, whether or not an upstream credential is configured.

## Push policy

Add a `git.push` rule to permit selected branch changes:

```yaml
authorization:
  rules:
    - name: read
      operations: [git.read]
      allow: sa.namespace == "agents"
    - name: safe-workspace-branches
      operations: [git.push]
      allow: >-
        sa.namespace == "agents" &&
        request.branch.startsWith("workspace/" + sa.name + "/") &&
        !request.force && !request.delete
```

```console
git -c "http.extraHeader=Authorization: Bearer $TOKEN" \
  -C application push origin HEAD:workspace/builder/change
```

Push clients also need `git.read` for receive-pack discovery. Only `refs/heads/*` may be pushed. Every ref command is evaluated with `request.branch`, `request.create`, `request.delete`, and `request.force`; one denial rejects the complete push before upstream access. 

Source Gateway validates the push, writes synchronously to the authoritative upstream with per-ref leases, then updates its mirror. If the push response is lost, it checks every affected upstream ref: a complete match is success; otherwise the client is told the outcome is unknown and must inspect the upstream before retrying. If only the local update fails after a confirmed upstream commit, Source Gateway reports success, stops reads, and requests an immediate repair.
