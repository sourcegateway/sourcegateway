#!/usr/bin/env bash
# Source Gateway <https://sourcegateway.dev>
# Copyright The Source Gateway Authors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
export NONO_NO_UPDATE_CHECK=1

# Envtest publishes its kubeconfig asynchronously after the process starts.
while [[ ! -s "$DEV_ROOT/kubeconfig" ]]; do sleep 0.1; done
export KUBECONFIG="$DEV_ROOT/kubeconfig"
export GIT_SSL_CAINFO="$DEV_ROOT/ca.crt"
exec bin/sourcegateway --config="$DEV_ROOT/config.jsonc" --allow-insecure-upstream=http://127.0.0.1:8333
