#!/usr/bin/env bash
# Source Gateway <https://sourcegateway.dev>
# Copyright The Source Gateway Authors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
export NONO_NO_UPDATE_CHECK=1

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$root/tmp"

# Keep every service, credential, socket, and generated repository inside an
# isolated directory, preserving logs on failure before tearing the stack down.
export DEV_ROOT
DEV_ROOT=$(mktemp -d "$root/tmp/e2e.XXXXXX")
socket="$DEV_ROOT/overmind.sock"
cache_mount=
failed=1
cleanup() {
	if [[ -S "$socket" ]]; then
		if [[ "$failed" == 1 ]]; then
			overmind echo -s "$socket" &
			log_pid=$!
			sleep 1
			kill "$log_pid" 2>/dev/null || true
			wait "$log_pid" 2>/dev/null || true
		fi
		overmind quit -s "$socket" || true
	fi
	if [[ -n "$cache_mount" ]]; then
		# SeaweedFS can retain the mounted directory briefly while shutting down.
		for _ in {1..50}; do
			if hdiutil detach -quiet "$cache_mount"; then
				cache_mount=
				break
			fi
			sleep 0.1
		done
		if [[ -n "$cache_mount" ]]; then
			hdiutil detach -quiet -force "$cache_mount"
		fi
	fi
	rm -rf "$DEV_ROOT"
}
trap cleanup EXIT INT TERM

# A dedicated filesystem makes the physical 10%-free cache invariant
# deterministic even when the development machine's main volume is nearly full.
if command -v hdiutil >/dev/null 2>&1; then
	cache_mount="$DEV_ROOT/storage-volume"
	mkdir -p "$cache_mount"
	hdiutil create -quiet -size 128m -fs APFS -volname SourceGatewayE2E "$DEV_ROOT/storage.dmg"
	hdiutil attach -quiet -nobrowse -mountpoint "$cache_mount" "$DEV_ROOT/storage.dmg"
	export SOURCEGATEWAY_DEV_STORAGE="$cache_mount/sourcegateway"
	export SOURCEGATEWAY_DEV_S3_STORAGE="$cache_mount/weed"
fi

# Prepare the local TLS and Kubernetes inputs, then start the localhost-only
# envtest, Git, S3, and Source Gateway processes under Overmind.
if [[ -z ${KUBEBUILDER_ASSETS:-} ]]; then
	KUBEBUILDER_ASSETS=$(go tool setup-envtest use -i -p path "${ENVTEST_VERSION:-1.36.2}")
fi
export KUBEBUILDER_ASSETS
"$root/scripts/dev/prepare.sh"
rm -f "$DEV_ROOT/kubeconfig"
overmind start -D -N -f "$root/Procfile.dev" -d "$root" -s "$socket"

# Wait for the complete routing snapshot to become ready before creating the
# Kubernetes resources and credentials used by the protocol tests.
for _ in {1..240}; do
	if curl --silent --fail http://127.0.0.1:18080/readyz >/dev/null; then break; fi
	sleep 0.25
done
curl --silent --fail http://127.0.0.1:18080/readyz >/dev/null
export KUBECONFIG="$DEV_ROOT/kubeconfig"
"$root/bin/e2e" bootstrap

token=$(cat "$DEV_ROOT/token")
gateway=https://127.0.0.1:8443/git/agents/application.git
git_client=(git -c "http.extraHeader=Authorization: Bearer $token" -c "http.sslCAInfo=$DEV_ROOT/ca.crt")
"${git_client[@]}" clone "$gateway" "$DEV_ROOT/clone" >/dev/null

# Change the upstream repository directly, reconcile it, and verify that a
# client fetch observes the refreshed pull-through mirror.
printf '%s\n' fetched >>"$DEV_ROOT/seed/README.md"
git -C "$DEV_ROOT/seed" add README.md
git -C "$DEV_ROOT/seed" commit -s -m fetch-test >/dev/null
git -C "$DEV_ROOT/seed" push origin main >/dev/null
"$root/bin/e2e" trigger
for _ in {1..120}; do
	if "${git_client[@]}" -C "$DEV_ROOT/clone" fetch origin >/dev/null 2>&1 && git -C "$DEV_ROOT/clone" show origin/main:README.md | grep -q fetched; then break; fi
	sleep 0.25
done
git -C "$DEV_ROOT/clone" show origin/main:README.md | grep -q fetched

# Exercise an allowed Git push through proc-receive and confirm that the
# authoritative upstream received the new branch.
git -C "$DEV_ROOT/clone" checkout -b allowed/e2e origin/main >/dev/null
printf '%s\n' pushed >"$DEV_ROOT/clone/pushed.txt"
git -C "$DEV_ROOT/clone" add pushed.txt
git -C "$DEV_ROOT/clone" -c user.name='Source Gateway E2E' -c user.email=e2e@sourcegateway.dev commit -s -m allowed >/dev/null
"${git_client[@]}" -C "$DEV_ROOT/clone" push origin allowed/e2e >/dev/null
git --git-dir="$DEV_ROOT/git/application.git" show-ref --verify refs/heads/allowed/e2e >/dev/null

# If the upstream accepts a push but the local mirror update fails, the client
# still sees success and the marker-driven repair restores a readable mirror.
"$root/bin/e2e" slow-refresh
mirror="${SOURCEGATEWAY_DEV_STORAGE:-$DEV_ROOT/storage}/git/agents/application.git"
cat >"$mirror/hooks/reference-transaction" <<EOF
#!/bin/sh
if [ "\$1" = prepared ]; then
	touch "$DEV_ROOT/local-update-rejected"
	exit 1
fi
EOF
chmod 0755 "$mirror/hooks/reference-transaction"
git -C "$DEV_ROOT/clone" checkout -b allowed/repair origin/main >/dev/null
printf '%s\n' repair >"$DEV_ROOT/clone/repair.txt"
git -C "$DEV_ROOT/clone" add repair.txt
git -C "$DEV_ROOT/clone" -c user.name='Source Gateway E2E' -c user.email=e2e@sourcegateway.dev commit -s -m repair >/dev/null
"${git_client[@]}" -C "$DEV_ROOT/clone" push origin allowed/repair >/dev/null
test -f "$DEV_ROOT/local-update-rejected"
git --git-dir="$DEV_ROOT/git/application.git" show-ref --verify refs/heads/allowed/repair >/dev/null
for _ in {1..120}; do
	if [[ ! -e "$mirror/.sourcegateway-refresh-required" ]] && \
		"${git_client[@]}" -C "$DEV_ROOT/clone" fetch origin allowed/repair >/dev/null 2>&1; then break; fi
	sleep 0.25
done
git -C "$DEV_ROOT/clone" show origin/allowed/repair:repair.txt | grep -q repair

# Verify policy rejection is returned to the Git client without modifying the
# upstream repository.
git -C "$DEV_ROOT/clone" checkout -b denied/e2e >/dev/null
if "${git_client[@]}" -C "$DEV_ROOT/clone" push origin denied/e2e >/dev/null 2>&1; then
	echo "denied Git push unexpectedly succeeded" >&2
	exit 1
fi

# Check the required observability signals, then delete the resources and
# confirm controller retirement removes local state without deleting upstream data.
metrics=$(curl --silent --fail http://127.0.0.1:18080/metrics)
grep -q '^sourcegateway_git_mirror_refresh_timestamp_seconds{' <<<"$metrics"
grep -q '^sourcegateway_object_cache_oldest_age_seconds{' <<<"$metrics"

"$root/bin/e2e" cleanup
for _ in {1..120}; do
	if [[ ! -e "$mirror" ]]; then break; fi
	sleep 0.25
done
test ! -e "$mirror"
git --git-dir="$DEV_ROOT/git/application.git" show-ref --verify refs/heads/allowed/repair >/dev/null

failed=0
echo "offline end-to-end test passed"
