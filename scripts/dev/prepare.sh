#!/usr/bin/env bash
# Source Gateway <https://sourcegateway.dev>
# Copyright The Source Gateway Authors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
export NONO_NO_UPDATE_CHECK=1
umask 077

: "${DEV_ROOT:?DEV_ROOT is required}"
storage_directory=${SOURCEGATEWAY_DEV_STORAGE:-$DEV_ROOT/storage}
s3_directory=${SOURCEGATEWAY_DEV_S3_STORAGE:-$DEV_ROOT/weed}
mkdir -p "$DEV_ROOT" "$DEV_ROOT/git" "$storage_directory" "$s3_directory" "$DEV_ROOT/secrets/dev-git" "$DEV_ROOT/secrets/dev-s3"
chmod 700 "$DEV_ROOT/secrets" "$DEV_ROOT/secrets/dev-git" "$DEV_ROOT/secrets/dev-s3"

# Generate one short-lived CA and localhost serving pair when the set is incomplete.
certificates_ready=true
for name in ca.crt ca.key tls.crt tls.key; do
	if [[ ! -s "$DEV_ROOT/$name" ]]; then
		certificates_ready=false
		break
	fi
done
if [[ "$certificates_ready" == false ]]; then
	certificate_work=$(mktemp -d "$DEV_ROOT/.certificates.XXXXXX")
	trap 'rm -rf "$certificate_work"' EXIT
	cat >"$certificate_work/openssl.cnf" <<'EOF'
[req]
distinguished_name = distinguished_name
prompt = no

[distinguished_name]
commonName = localhost

[v3_ca]
basicConstraints = critical, CA:true
keyUsage = critical, digitalSignature, keyCertSign
subjectKeyIdentifier = hash

[v3_server]
basicConstraints = critical, CA:false
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @server_names

[server_names]
DNS.1 = localhost
IP.1 = 127.0.0.1
EOF
	openssl genrsa -out "$certificate_work/ca.key" 2048 >/dev/null
	openssl req -new -x509 -sha256 -days 2 \
		-key "$certificate_work/ca.key" \
		-out "$certificate_work/ca.crt" \
		-set_serial "0x$(openssl rand -hex 16)" \
		-subj '/CN=Source Gateway development CA' \
		-config "$certificate_work/openssl.cnf" \
		-extensions v3_ca
	openssl genrsa -out "$certificate_work/tls.key" 2048 >/dev/null
	openssl req -new -sha256 \
		-key "$certificate_work/tls.key" \
		-out "$certificate_work/tls.csr" \
		-subj '/CN=localhost' \
		-config "$certificate_work/openssl.cnf"
	openssl x509 -req -sha256 -days 2 \
		-in "$certificate_work/tls.csr" \
		-CA "$certificate_work/ca.crt" \
		-CAkey "$certificate_work/ca.key" \
		-set_serial "0x$(openssl rand -hex 16)" \
		-extfile "$certificate_work/openssl.cnf" \
		-extensions v3_server \
		-out "$certificate_work/tls.crt"
	for name in ca.crt ca.key tls.crt tls.key; do
		mv "$certificate_work/$name" "$DEV_ROOT/$name"
	done
	rm -rf "$certificate_work"
	trap - EXIT
fi

# Envtest signs local ServiceAccount tokens with this independent RSA key.
if [[ ! -s "$DEV_ROOT/service-account.key" ]]; then
	openssl genrsa -out "$DEV_ROOT/service-account.key" 2048 >/dev/null
fi
chmod 600 "$DEV_ROOT/ca.crt" "$DEV_ROOT/ca.key" "$DEV_ROOT/tls.crt" "$DEV_ROOT/tls.key" "$DEV_ROOT/service-account.key"

printf '%s\n' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=' >"$DEV_ROOT/secrets/encryption-key"
printf '%s\n' dev >"$DEV_ROOT/secrets/dev-git/username"
printf '%s\n' dev >"$DEV_ROOT/secrets/dev-git/token"
printf '%s\n' dev >"$DEV_ROOT/secrets/dev-s3/access-key-id"
printf '%s\n' dev >"$DEV_ROOT/secrets/dev-s3/secret-access-key"
chmod 600 "$DEV_ROOT/secrets/encryption-key" "$DEV_ROOT/secrets/dev-git/"* "$DEV_ROOT/secrets/dev-s3/"*

cat >"$DEV_ROOT/config.jsonc" <<EOF
{
  "tokenAudience": "sourcegateway.dev",
  "objectCacheCapacity": "30Mi",
  "maxObjectSize": "15Mi",
  "credentialsDirectory": "$DEV_ROOT/secrets",
  "listenAddress": "127.0.0.1:8443",
  "metricsAddress": "127.0.0.1:18080",
  "storageDirectory": "$storage_directory",
  "tlsCertificateFile": "$DEV_ROOT/tls.crt",
  "tlsPrivateKeyFile": "$DEV_ROOT/tls.key",
  "credentials": {
    "gitRepository": {"dev-git": {"kind": "githubUser", "namespaces": ["agents"], "origin": "https://127.0.0.1:9443"}},
    "objectStorageBucket": {"dev-s3": {"kind": "awsAccessKey", "namespaces": ["agents"], "origin": "http://127.0.0.1:8333"}}
  }
}
EOF

if [[ ! -d "$DEV_ROOT/git/application.git" ]]; then
	git init --bare "$DEV_ROOT/git/application.git" >/dev/null
	git -C "$DEV_ROOT/git/application.git" config http.receivepack true
	seed="$DEV_ROOT/seed"
	git init -b main "$seed" >/dev/null
	git -C "$seed" config user.name 'Source Gateway E2E'
	git -C "$seed" config user.email 'e2e@sourcegateway.dev'
	printf '%s\n' initial >"$seed/README.md"
	git -C "$seed" add README.md
	git -C "$seed" commit -s -m initial >/dev/null
	git -C "$seed" remote add origin "$DEV_ROOT/git/application.git"
	git -C "$seed" push origin main >/dev/null
	git --git-dir="$DEV_ROOT/git/application.git" symbolic-ref HEAD refs/heads/main
fi
