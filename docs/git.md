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
