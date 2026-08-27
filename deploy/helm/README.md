# Source Gateway Helm chart

This chart installs one Source Gateway replica, RBAC, cache storage, a Service,
and an ingress NetworkPolicy. It can also install the Source Gateway CRDs.
Kubernetes 1.37 and Linux nodes supporting Pod user namespaces are required.

## Install

For the encryption key, serving TLS, and each file-backed credential, configure
exactly one `secretProviderClass` or `secretName`. Secrets Store CSI is preferred;
enable automatic rotation and configure files with mode `0400`. Native
Kubernetes Secrets are also mounted with mode `0400`. When using native Secrets,
enable Kubernetes encryption at rest because their contents are stored in etcd.
Both volume types are mounted whole, without `subPath`, so file updates remain
visible.

The encryption source must publish `encryption-key` containing standard base64
for 32 bytes. The TLS source must publish `tls.crt` and `tls.key`. Credential
sources use the filenames documented in the main deployment guide.

The chart sets `hostUsers: false` and runs Source Gateway as UID 0 inside the
Pod's user namespace. Kubernetes maps that UID to a unique unprivileged host UID
while supported secret mounts expose their private files to the in-container UID
without copying or interrupting rotation. Every eligible node must support
idmapped mounts; do not override the chart's root user settings.

CRDs are disabled by default so their lifecycle can be managed independently.
Install or upgrade the CRD release first:

```console
helm upgrade --install sourcegateway-crds ./deploy/helm \
  --namespace sourcegateway --create-namespace \
  --set crds.enabled=true --set enabled=false

helm upgrade --install sourcegateway ./deploy/helm \
  --namespace sourcegateway \
  -f deploy/helm/values-example.yaml
```

CRDs are rendered as regular Helm templates so the CRD release upgrades them.
Set `crds.enabled=true` on the gateway release only when that single release
should own both the application and its CRDs.

Runtime configuration is strict JSON rendered into a ConfigMap. Credentials may
declare a trusted `origin`; GitHub, GCS, and regional AWS S3 have safe defaults.
Selected resources cannot send those credentials elsewhere. The chart-only
`secretProviderClass` and `secretName` keys are intentionally removed from that
JSON. Config and mount changes update pod checksum annotations. The main Service
exposes only application TLS on 443. The Pod's TLS operational port 9443 remains
private for health probes. Use a `PodMonitor` or create a separate Service for
metrics, and restrict `networkPolicy.metricsIngressFrom` to the collector.
Provider egress remains operator-owned.

See `values.yaml` for every setting and `values-example.yaml` for all
credential mount forms. When persistence is enabled, keep `persistence.size`
larger than `config.objectCacheCapacity` to leave space for Git mirrors and cache
headroom. The PVC is enabled by default. Set `persistence.enabled=false` to use
ephemeral `emptyDir` storage when rebuilding all local data after pod replacement
is acceptable. Object writes default to a 15 MiB limit; increase
`config.maxObjectSize` when workloads require larger objects, or set it to
`null` to rely on upstream limits and transfer timeouts.
