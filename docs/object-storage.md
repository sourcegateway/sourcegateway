---
title: "Object Storage"
weight: 40
description: "Use Source Gateway's deliberately limited path-style S3-compatible interface."
---

# Object Storage

Source Gateway exposes a deliberately limited S3-compatible HTTP surface backed by S3 or GCS. It is not a general S3 endpoint. Clients must use path-style bucket URLs, disable request signing, use TLS, and send `Authorization: Bearer <projected-token>`; SigV4, presigned URLs, and virtual-host bucket names are unsupported.

The target client is an AWS SDK or S3-compatible tool that supports anonymous, path-style requests with a custom HTTP header. End-to-end tests cover the AWS SDK for Go, and [rclone](https://rclone.org/); the curl example below is more designed to demonstrate the requirements of a HTTP request rather than being expected to be used in production. Note that some S3 tools cannot disable signing or add a bearer header and therefore cannot connect.

## Find the bucket

Get the stable downstream bucket name from status:

```console
kubectl -n agents get objectstoragebucket artifacts \
  -o jsonpath='{.status.bucketName}{"\n"}'
```

## rclone example

Use rclone's on-the-fly S3 backend for a quick listing without storing credentials:

```console
TOKEN=$(cat /var/run/secrets/sourcegateway.dev/token)
BUCKET=$(kubectl -n agents get objectstoragebucket artifacts -o jsonpath='{.status.bucketName}')

rclone lsf ":s3:$BUCKET/workspaces/" \
  --s3-provider=Other \
  --s3-endpoint=https://sourcegateway.sourcegateway.svc \
  --s3-env-auth=false --s3-no-check-bucket --s3-list-version=2 \
  --s3-access-key-id= --s3-secret-access-key= \
  --header="Authorization: Bearer $TOKEN"
```

Apply the same endpoint, path-style, anonymous-credential, and bearer-header settings when constructing an SDK client.

## curl example

For direct HTTP requests:

```console
TOKEN=$(cat /var/run/secrets/sourcegateway.dev/token)
BUCKET=$(kubectl -n agents get objectstoragebucket artifacts -o jsonpath='{.status.bucketName}')
BASE="https://sourcegateway.sourcegateway.svc/$BUCKET"

curl -H "Authorization: Bearer $TOKEN" "$BASE?list-type=2&prefix=workspaces/"
curl -H "Authorization: Bearer $TOKEN" "$BASE/workspaces/builder/result.txt"
curl -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "x-amz-checksum-sha256: <base64-sha256>" \
  --data-binary @result.txt "$BASE/workspaces/builder/result.txt"
curl -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE/workspaces/builder/result.txt"
```

## Supported operations

Supported operations are `ListBuckets`, `HeadBucket`, `ListObjectsV2`, `HeadObject`, `GetObject`, `PutObject`, `DeleteObject`, `DeleteObjects`, same-bucket `CopyObject`, and the create, upload-part, complete, and abort multipart operations. They map to the `bucket.*` and `object.*` policy operations in [Authentication and Access Control](auth.md).

`PutObject` accepts a fixed `Content-Length`, HTTP chunked transfer encoding, and the `aws-chunked` checksum framing used by current AWS SDKs. Checksums are optional; SHA-256, MD5, CRC32, and CRC32C may be supplied in a header or trailer. `Expect: 100-continue` is supported. With a configured size limit, Source Gateway stages and validates each upload before contacting the upstream. Without a limit, ordinary fixed-length uploads can stream directly; chunked, `aws-chunked`, and checksummed uploads remain staged so validation finishes before the upstream write.

For S3 multipart clients, non-final parts must be at least 5 MiB and the completed object must fit the configured size limit. Parts are staged locally and assembled as one provider-neutral upstream write on completion. Incomplete uploads are process-local and expire after two hours; abort them promptly to release their local staging space.

## Limits and large objects

The `maxObjectSize` setting limits single puts, copies, and completed multipart objects. It defaults to 15 MiB to remain below Nono's current 16 MiB buffered HTTP/1 proxy limit. Set it to `null` to rely on upstream limits and Source Gateway's transfer timeouts instead.

Multipart upload makes large transfers resumable but does not bypass a configured final object limit. For larger objects, increase or disable `maxObjectSize`, split the data into objects with an application-level manifest, or upload directly to the upstream provider. Splitting retains Source Gateway policy and immediate cache coherence. A direct upload requires separate provider credentials, bypasses Source Gateway write policy, and may leave a previously cached object visible until its TTL expires; lists still query upstream directly.

Other limits are 1,000 list or multi-delete entries, 10,000 multipart parts, 128 concurrent multipart uploads per replica, and one byte range. Listing multipart uploads and parts, ACLs, versioning, and bucket mutation are unsupported. Keys are 1–1,024 ASCII bytes, start alphanumerically, use `[A-Za-z0-9._/-]`, and cannot contain empty, `.` or `..` path segments.

## Cache consistency

Lists always query the authoritative provider. Writes are acknowledged only after the provider commits them. Puts populate the cache when capacity permits, while larger puts, copies, and deletes evict affected entries before success is reported. Operations on the same key are serialized, and startup SHA-256 verifies cached bytes. Cache admission never blocks a write. The TTL therefore mainly bounds changes made directly upstream; if every write passes through Source Gateway, subsequent reads do not use an older cached value. Failed transfers do not expose partial cache entries.

If an upstream response is lost after a mutation was dispatched, Source Gateway cannot safely tell the client to retry. It evicts the affected cache entry and returns HTTP `409` with `OperationOutcomeUnknown`; inspect the authoritative object before retrying. Multi-object delete reports an unknown outcome per key.
