---
title: "Getting Started"
weight: 10
description: "Run a disposable Source Gateway installation on a local Kind cluster."
---

# Getting Started

This guide creates a disposable Source Gateway installation on a local [Kind](https://kind.sigs.k8s.io/) cluster, then demo's using both an object storage bucket and a Git repository. You need Kind, Helm, kubectl, curl, and OpenSSL. Kind must use Kubernetes 1.37 on a Linux 6.3+ host or VM with Pod user-namespace support.

## Create the cluster

```console
kind create cluster --name sourcegateway --image kindest/node:v1.37.0
kubectl create namespace sourcegateway
```

Kind currently needs an additional sysfs mount before it can start Pods that
use user namespaces. For a Docker-backed Kind cluster, prepare the node:

```console
docker exec "$(kind get nodes --name sourcegateway)" \
  sh -c 'mkdir -p /mnt/sysfs && (mountpoint -q /mnt/sysfs || mount -t sysfs none /mnt/sysfs)'
```

## Create test secrets

For this local installation, create a short-lived self-signed certificate and native Kubernetes Secrets. The Git credential files are intentionally empty because the disposable Git server uses anonymous access:

```console
openssl rand -base64 32 | kubectl -n sourcegateway create secret generic \
  sourcegateway-encryption-key --from-file=encryption-key=/dev/stdin

WORK=$(mktemp -d)
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
  -subj '/CN=sourcegateway.sourcegateway.svc' \
  -addext 'subjectAltName=DNS:sourcegateway.sourcegateway.svc,DNS:localhost,IP:127.0.0.1' \
  -keyout "$WORK/tls.key" -out "$WORK/tls.crt"

kubectl -n sourcegateway create secret tls sourcegateway-serving-tls \
  --cert="$WORK/tls.crt" --key="$WORK/tls.key"

kubectl create configmap sourcegateway-demo-ca --from-file=ca.crt="$WORK/tls.crt"

kubectl -n sourcegateway create secret generic sourcegateway-demo-s3 \
  --from-literal=access-key-id=demo \
  --from-literal=secret-access-key=demo

kubectl -n sourcegateway create secret generic sourcegateway-demo-git \
  --from-literal=username= \
  --from-literal=token=
```

## Install Source Gateway

CRDs are disabled by default so they can have an independent lifecycle. Install the chart twice, using ephemeral storage for this test. This local Kind setup also disables the chart's ingress NetworkPolicy because Kind networking blocks return traffic to the user-namespaced Pod when that policy is installed; keep the policy enabled in production:

```console
VERSION=0.1.0
CHART=oci://ghcr.io/sourcegateway/charts/sourcegateway

helm upgrade --install sourcegateway-crds "$CHART" \
  --version "$VERSION" \
  --namespace sourcegateway \
  --set crds.enabled=true --set enabled=false

cat >"$WORK/values.yaml" <<'EOF'
persistence:
  enabled: false
networkPolicy:
  enabled: false
extraArgs:
  - --allow-insecure-upstream=http://seaweedfs.sourcegateway.svc:8333
  - --allow-insecure-upstream=http://git-server.sourcegateway.svc:23232
encryptionKey:
  secretName: sourcegateway-encryption-key
servingTLS:
  secretName: sourcegateway-serving-tls
config:
  credentials:
    gitRepository:
      git-demo:
        kind: githubUser
        namespaces: [default]
        origin: http://git-server.sourcegateway.svc:23232
        secretName: sourcegateway-demo-git
    objectStorageBucket:
      s3-demo:
        kind: awsAccessKey
        namespaces: [default]
        origin: http://seaweedfs.sourcegateway.svc:8333
        secretName: sourcegateway-demo-s3
EOF

helm upgrade --install sourcegateway "$CHART" \
  --version "$VERSION" \
  --namespace sourcegateway \
  --values "$WORK/values.yaml"
```

Wait for the Deployment:

```console
kubectl -n sourcegateway rollout status deployment/sourcegateway --timeout=2m
```

## Verify the installation

Forward the TLS operational endpoint in one terminal:

```console
kubectl -n sourcegateway port-forward deployment/sourcegateway 9443:9443
```

Then check readiness from another:

```console
curl --cacert "$WORK/tls.crt" https://localhost:9443/readyz
```

The response is `ok`.

## Test object storage

Run a single disposable SeaweedFS Pod as the demo's S3 upstream:

```console
kubectl -n sourcegateway run seaweedfs \
  --image=chrislusf/seaweedfs:4.44 \
  --port=8333 -- \
  mini -dir=/tmp/seaweedfs -ip=127.0.0.1 -ip.bind=0.0.0.0 \
  -s3.port=8333 -filer.port=8888 -master.port=9333 -volume.port=18081 \
  -master.telemetry=false

kubectl -n sourcegateway expose pod seaweedfs \
  --name=seaweedfs --port=8333
```

Forward SeaweedFS to the host, then create its authoritative bucket and seed one object directly upstream:

```console
kubectl -n sourcegateway port-forward service/seaweedfs 8333:8333 \
  >/tmp/sourcegateway-seaweedfs.log 2>&1 &
PORT_FORWARD=$!
curl -sS --retry 30 --retry-delay 1 --retry-connrefused \
  -o /dev/null http://localhost:8333

curl -fsS -X PUT http://localhost:8333/demo
printf 'hello from upstream\n' | \
  curl -fsS -X PUT --data-binary @- http://localhost:8333/demo/hello.txt
```

Expose that bucket through Source Gateway. The policy grants only the demo ServiceAccount access:

```console
kubectl apply -f - <<'EOF'
apiVersion: sourcegateway.dev/v1alpha1
kind: ObjectStorageBucket
metadata:
  name: demo
  namespace: default
spec:
  backend:
    provider: s3
    endpoint: http://seaweedfs.sourcegateway.svc:8333
    region: us-east-1
    bucket: demo
    credential: s3-demo
  cache:
    capacity: 100Mi
    ttl: 5m
  authorization:
    rules:
      - name: demo
        operations: [bucket.head, object.list, object.read, object.write, object.delete]
        allow: 'sa.namespace == "default" && sa.name == "sourcegateway-demo"'
EOF

kubectl wait --for=condition=Ready objectstoragebucket/demo --timeout=2m
BUCKET=$(kubectl get objectstoragebucket demo -o jsonpath='{.status.bucketName}')
```

Create an rclone Pod with the test CA and a projected Source Gateway token. It receives no SeaweedFS credentials:

```console
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: sourcegateway-demo
---
apiVersion: v1
kind: Pod
metadata:
  name: sourcegateway-demo
spec:
  serviceAccountName: sourcegateway-demo
  automountServiceAccountToken: false
  containers:
    - name: rclone
      image: rclone/rclone:1.71.1
      command: ["/bin/sh", "-c", "sleep 1d"]
      volumeMounts:
        - { name: token, mountPath: /var/run/sourcegateway, readOnly: true }
        - { name: ca, mountPath: /etc/sourcegateway, readOnly: true }
  volumes:
    - name: token
      projected:
        defaultMode: 0400
        sources:
          - serviceAccountToken:
              path: token
              audience: sourcegateway.dev
              expirationSeconds: 3600
    - name: ca
      configMap:
        name: sourcegateway-demo-ca
EOF

kubectl wait --for=condition=Ready pod/sourcegateway-demo --timeout=2m
```

Open a shell in the container, passing it the generated gateway bucket name:

```console
kubectl exec -it sourcegateway-demo -- env BUCKET="$BUCKET" /bin/sh
```

Then sync from Source Gateway, change the existing file, add another, and sync back:

```console
TOKEN=$(cat /var/run/sourcegateway/token)
s3sync() {
  rclone sync "$@" --s3-provider=Other \
    --s3-endpoint=https://sourcegateway.sourcegateway.svc \
    --s3-env-auth=false --s3-no-check-bucket --s3-list-version=2 \
    --s3-access-key-id= --s3-secret-access-key= \
    --header="Authorization: Bearer $TOKEN" \
    --ca-cert=/etc/sourcegateway/ca.crt
}

s3sync ":s3:$BUCKET" /work
cat /work/hello.txt
printf 'updated through Source Gateway\n' >/work/hello.txt
printf 'a second object\n' >/work/second.txt
s3sync /work ":s3:$BUCKET"
exit
```

Finally, read the authoritative objects directly from SeaweedFS on the host:

```console
curl -fsS http://localhost:8333/demo/hello.txt
curl -fsS http://localhost:8333/demo/second.txt
```

The output shows both changes made through Source Gateway.

## Test Git

Run a disposable [Soft Serve](https://github.com/charmbracelet/soft-serve) Git server with anonymous administration enabled for this isolated test cluster:

```console
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: git-server
  namespace: sourcegateway
  labels:
    app: git-server
spec:
  containers:
    - name: git
      image: charmcli/soft-serve:v0.12.2
      env:
        - { name: SOFT_SERVE_DEFAULT_REPO, value: hello }
        - { name: SOFT_SERVE_ANON_ACCESS, value: admin-access }
        - { name: SOFT_SERVE_ALLOW_KEYLESS, value: "true" }
      ports:
        - { name: http, containerPort: 23232 }
      readinessProbe:
        tcpSocket: { port: http }
  restartPolicy: Never
---
apiVersion: v1
kind: Service
metadata:
  name: git-server
  namespace: sourcegateway
spec:
  selector:
    app: git-server
  ports:
    - { name: http, port: 23232, targetPort: http }
EOF

kubectl -n sourcegateway wait --for=condition=Ready pod/git-server --timeout=2m
```

Seed its authoritative repository directly with one file:

```console
kubectl -n sourcegateway exec git-server -- sh -c '
  set -eu
  WORK=$(mktemp -d)
  git -C "$WORK" init -b main
  printf "hello from upstream\n" >"$WORK/hello.txt"
  git -C "$WORK" add hello.txt
  git -C "$WORK" -c user.name=Demo -c user.email=demo@example.invalid \
    commit -m "Add hello file"
  git -C "$WORK" push http://localhost:23232/hello.git main
'
```

Expose the repository through Source Gateway. The policy grants only the demo ServiceAccount access and rejects forced pushes and branch deletion:

```console
kubectl apply -f - <<'EOF'
apiVersion: sourcegateway.dev/v1alpha1
kind: GitRepository
metadata:
  name: hello
  namespace: default
spec:
  upstream:
    url: http://git-server.sourcegateway.svc:23232/hello.git
    credential: git-demo
  mirror:
    refreshInterval: 5s
    maxStaleness: 1m
  authorization:
    rules:
      - name: read
        operations: [git.read]
        allow: 'sa.namespace == "default" && sa.name == "sourcegateway-git-demo"'
      - name: push
        operations: [git.push]
        allow: >-
          sa.namespace == "default" &&
          sa.name == "sourcegateway-git-demo" &&
          !request.force && !request.delete
EOF

kubectl wait --for=condition=Ready gitrepository/hello --timeout=2m
```

Create a Git client Pod with the test CA and a projected Source Gateway token. It receives no upstream Git credentials:

```console
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: sourcegateway-git-demo
---
apiVersion: v1
kind: Pod
metadata:
  name: sourcegateway-git-demo
spec:
  serviceAccountName: sourcegateway-git-demo
  automountServiceAccountToken: false
  containers:
    - name: git
      image: alpine/git:2.49.1
      command: ["/bin/sh", "-c", "sleep 1d"]
      workingDir: /work
      volumeMounts:
        - { name: token, mountPath: /var/run/sourcegateway, readOnly: true }
        - { name: ca, mountPath: /etc/sourcegateway, readOnly: true }
  volumes:
    - name: token
      projected:
        defaultMode: 0400
        sources:
          - serviceAccountToken:
              path: token
              audience: sourcegateway.dev
              expirationSeconds: 3600
    - name: ca
      configMap:
        name: sourcegateway-demo-ca
EOF

kubectl wait --for=condition=Ready pod/sourcegateway-git-demo --timeout=2m
kubectl exec -it sourcegateway-git-demo -- /bin/sh
```

Inside the container, clone through Source Gateway, read the existing file, add another, and push it back:

```console
TOKEN=$(cat /var/run/sourcegateway/token)
export GIT_CONFIG_COUNT=2
export GIT_CONFIG_KEY_0=http.extraHeader
export GIT_CONFIG_VALUE_0="Authorization: Bearer $TOKEN"
export GIT_CONFIG_KEY_1=http.sslCAInfo
export GIT_CONFIG_VALUE_1=/etc/sourcegateway/ca.crt

git clone https://sourcegateway.sourcegateway.svc/git/default/hello.git
cd hello
cat hello.txt
git config user.name Demo
git config user.email demo@example.invalid
printf 'added through Source Gateway\n' >second.txt
git add second.txt
git commit -m 'Add second file'
git push origin main
exit
```

Finally, read the authoritative commit directly from the Git server:

```console
kubectl -n sourcegateway exec git-server -- sh -c '
  WORK=$(mktemp -d)
  git clone --quiet http://localhost:23232/hello.git "$WORK/verify"
  cat "$WORK/verify/second.txt"
'
```

The output is `added through Source Gateway`.

## Clean up

Delete the test cluster when finished:

```console
kill "$PORT_FORWARD" 2>/dev/null || true
kind delete cluster --name sourcegateway
rm -rf "$WORK"
```

## What's next

This quickstart deliberately uses disposable storage, native Kubernetes Secrets, and a self-signed certificate. For production:

- Omit `persistence.enabled=false` so the default PVC preserves mirrors and caches across Pod replacement.
- Prefer Secrets Store CSI volumes with automatic rotation instead of native Secrets, which store secrets as plaintext in etcd.
- Use a certificate issued by your cluster PKI and restrict network access to port 9443.
- If you use native Secrets, enable Kubernetes encryption at rest because their contents are stored in etcd.

See [Deployment](deployment.md) for production requirements and credential configuration.
