# Source Gateway <https://sourcegateway.dev>
# Copyright The Source Gateway Authors
# SPDX-License-Identifier: Apache-2.0

.DEFAULT_GOAL := help

BINDIR := bin
BINARY := $(BINDIR)/sourcegateway
BUILDVARS := github.com/sourcegateway/sourcegateway/internal/buildvars
OCIMAGE ?= ocimage
IMAGE_ARCHES ?= amd64,arm64
IMAGE_PLATFORMS := $(shell printf '%s' '$(IMAGE_ARCHES)' | tr -d ' ' | sed 's/[^,][^,]*/linux\/&/g')
IMAGE_TAGS ?= ghcr.io/sourcegateway/sourcegateway:$(VERSION)
IMAGE_PUSH ?= false
IMAGE_SBOM ?= false
VERSION := $(shell git describe --tags --exact-match 2>/dev/null || echo dev)
BUILD_DATE := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
COMMIT_HASH := $(shell git rev-parse --verify HEAD 2>/dev/null || echo unknown)
COMMIT_DATE := $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)
COMMIT_BRANCH := $(shell git branch --show-current 2>/dev/null || echo unknown)
DEV_ROOT ?= $(CURDIR)/tmp/dev
KUBERNETES_VERSION := 1.37.0
# Kubernetes 1.37 envtest assets are not published yet; keep this independently overrideable.
ENVTEST_VERSION ?= 1.36.2
KUBECONFORM_SCHEMA_VERSION := 1.30.0
export PATH := $(CURDIR)/bin:$(PATH)
export NONO_NO_UPDATE_CHECK := 1

.PHONY: help setup setup-tools fmt lint precommit test build build-dev image-binaries image generate validate-generated helm dev e2e clean

help: ## Show available targets
	@echo "Usage: make <target>"
	@awk 'BEGIN {FS = ":.*?## "} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: setup-tools ## Install pinned development tools and Git hooks
	@mkdir -p .git/hooks
	@cp scripts/git-hooks/pre-commit .git/hooks/pre-commit
	@cp scripts/git-hooks/commit-msg .git/hooks/commit-msg
	@chmod +x .git/hooks/pre-commit .git/hooks/commit-msg
	@echo "Source Gateway development tools are ready."

setup-tools: ## Install pinned development tools with mise and Go
	@command -v mise >/dev/null 2>&1 || { echo "mise is required" >&2; exit 1; }
	@command -v git >/dev/null 2>&1 || { echo "git is required" >&2; exit 1; }
	@command -v openssl >/dev/null 2>&1 || { echo "OpenSSL is required" >&2; exit 1; }
	@command -v tmux >/dev/null 2>&1 || { echo "tmux is required" >&2; exit 1; }
	@mise install
	@mise exec -- go tool setup-envtest use $(ENVTEST_VERSION) >/dev/null

fmt: ## Format Go source files
	@go fmt ./...

lint: ## Run golangci-lint
	@go tool golangci-lint run --timeout=5m

precommit: ## Run fast read-only formatting, generation, and lint checks
	@unformatted=$$(gofmt -l $$(find . -name '*.go' -not -path './bin/*' -not -path './tmp/*')); \
		test -z "$$unformatted" || { echo "These files need gofmt:"; echo "$$unformatted"; exit 1; }
	@$(MAKE) validate-generated
	@go vet ./...

test: ## Run all unit tests with the race detector
	@go test -race ./...

build: ## Build Source Gateway with immutable build metadata
	@mkdir -p $(BINDIR)
	@CGO_ENABLED=0 go build -trimpath -ldflags "-s -w \
		-X $(BUILDVARS).buildVersion=$(VERSION) \
		-X $(BUILDVARS).buildDate=$(BUILD_DATE) \
		-X $(BUILDVARS).commitHash=$(COMMIT_HASH) \
		-X $(BUILDVARS).commitDate=$(COMMIT_DATE) \
		-X $(BUILDVARS).commitBranch=$(COMMIT_BRANCH)" \
		-o $(BINARY) ./cmd/sourcegateway

build-dev: build ## Build the binary and offline development helpers
	@go build -trimpath -o $(BINDIR)/dev-envtest ./cmd/dev-envtest
	@go build -trimpath -o $(BINDIR)/dev-git ./cmd/dev-git
	@go build -trimpath -o $(BINDIR)/e2e ./tests/e2e

image-binaries: ## Cross-compile binaries for the production image
	@set -eu; for arch in $$(printf '%s' '$(IMAGE_ARCHES)' | tr ',' ' '); do \
		$(MAKE) build GOOS=linux GOARCH="$$arch" BINDIR="bin/linux_$$arch" \
			VERSION="$(VERSION)" BUILD_DATE="$(BUILD_DATE)" COMMIT_HASH="$(COMMIT_HASH)" \
			COMMIT_DATE="$(COMMIT_DATE)" COMMIT_BRANCH="$(COMMIT_BRANCH)"; \
	done

image: image-binaries ## Package the production image with ocimage
	$(OCIMAGE) build \
		--file images/sourcegateway/Containerfile \
		--platform $(IMAGE_PLATFORMS) \
		$(foreach tag,$(IMAGE_TAGS),--tag $(tag)) \
		--build-arg VERSION=$(VERSION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--build-arg COMMIT_HASH=$(COMMIT_HASH) \
		--sbom=$(IMAGE_SBOM) \
		$(if $(filter true,$(IMAGE_PUSH)),--push,) \
		.

generate: ## Regenerate API code, CRDs, RBAC, and packaged chart CRDs
	@rm -rf deploy/helm/templates/crds
	@mkdir -p deploy/crds deploy/rbac deploy/helm/templates/crds
	@go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	@go tool controller-gen crd:crdVersions=v1 rbac:roleName=sourcegateway-role paths=./... \
		output:crd:artifacts:config=deploy/crds output:rbac:artifacts:config=deploy/rbac
	@for source in deploy/crds/*.yaml; do \
		target="deploy/helm/templates/crds/$$(basename "$$source")"; \
		{ printf '%s\n' '{{- if .Values.crds.enabled }}'; cat "$$source"; printf '%s\n' '{{- end }}'; } >"$$target"; \
	done

validate-generated: ## Verify generated and packaged files are present and synchronized
	@test -s api/v1alpha1/zz_generated.deepcopy.go
	@for source in deploy/crds/*.yaml; do \
		target="deploy/helm/templates/crds/$$(basename "$$source")"; \
		{ printf '%s\n' '{{- if .Values.crds.enabled }}'; cat "$$source"; printf '%s\n' '{{- end }}'; } | cmp -s - "$$target" || exit 1; \
	done

helm: ## Lint, render, and strictly validate the Helm chart
	@helm lint deploy/helm --kube-version $(KUBERNETES_VERSION) -f deploy/helm/values-example.yaml
	@helm lint deploy/helm --kube-version $(KUBERNETES_VERSION) --set crds.enabled=true --set enabled=false
	@mkdir -p tmp
	@helm template sourcegateway deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway -f deploy/helm/values-example.yaml \
		--set crds.enabled=true >tmp/helm.yaml
	@helm template sourcegateway-crds deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway \
		--set crds.enabled=true --set enabled=false >tmp/helm-crds.yaml
	@helm template sourcegateway deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway \
		-f deploy/helm/values-example.yaml >tmp/helm-gateway.yaml
	@helm template sourcegateway deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway -f deploy/helm/values-example.yaml \
		--set persistence.enabled=false \
		--set 'extraArgs[0]=--allow-insecure-upstream=http://seaweedfs.sourcegateway.svc:8333' >tmp/helm-ephemeral.yaml
	@helm template sourcegateway deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway -f deploy/helm/values-example.yaml \
		--set encryptionKey.secretProviderClass= --set encryptionKey.secretName=sourcegateway-encryption-key \
		--set servingTLS.secretProviderClass= --set servingTLS.secretName=sourcegateway-serving-tls \
		--set config.credentials.gitRepository.example-github.secretProviderClass= \
		--set config.credentials.gitRepository.example-github.secretName=example-github >tmp/helm-secrets.yaml
	@helm template sourcegateway deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway -f deploy/helm/values-example.yaml \
		--set 'networkPolicy.metricsIngressFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring' >tmp/helm-metrics.yaml
	@test "$$(grep -c '^kind: PersistentVolumeClaim$$' tmp/helm.yaml)" -eq 1
	@test "$$(grep -c '^kind: CustomResourceDefinition$$' tmp/helm.yaml)" -eq 2
	@test "$$(grep -c '^kind: CustomResourceDefinition$$' tmp/helm-crds.yaml)" -eq 2
	@test "$$(grep -c '^kind:' tmp/helm-crds.yaml)" -eq 2
	@test "$$(grep -c '^kind: CustomResourceDefinition$$' tmp/helm-gateway.yaml || true)" -eq 0
	@test "$$(grep -c '^kind: Deployment$$' tmp/helm-gateway.yaml)" -eq 1
	@test "$$(grep -c '^kind: Service$$' tmp/helm-gateway.yaml)" -eq 1
	@test "$$(grep -c '^kind: Service$$' tmp/helm-metrics.yaml)" -eq 1
	@grep -q 'kubernetes.io/metadata.name: monitoring' tmp/helm-metrics.yaml
	@grep -q 'port: metrics' tmp/helm-metrics.yaml
	@! grep -q 'sourcegateway-sourcegateway' tmp/helm-gateway.yaml
	@test "$$(grep -c '^kind: PersistentVolumeClaim$$' tmp/helm-ephemeral.yaml || true)" -eq 0
	@grep -q '^[[:space:]]*emptyDir: {}$$' tmp/helm-ephemeral.yaml
	@grep -q '^[[:space:]]*- "--allow-insecure-upstream=http://seaweedfs.sourcegateway.svc:8333"$$' tmp/helm-ephemeral.yaml
	@grep -q '^[[:space:]]*hostUsers: false$$' tmp/helm.yaml
	@grep -q '^[[:space:]]*runAsUser: 0$$' tmp/helm.yaml
	@test "$$(grep -c '^[[:space:]]*- name: sourcegateway$$' tmp/helm.yaml)" -eq 1
	@! grep -q 'secret-projection\|sync-secrets\|CHOWN' tmp/helm.yaml
	@! grep -q 'secrets-store.csi.k8s.io' tmp/helm-secrets.yaml
	@test "$$(grep -c '^[[:space:]]*defaultMode: 0400$$' tmp/helm-secrets.yaml)" -eq 4
	@if helm template sourcegateway deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway -f deploy/helm/values-example.yaml \
		--set encryptionKey.secretName=duplicate >/dev/null 2>&1; then echo "expected duplicate secret source rejection" >&2; exit 1; fi
	@if helm template sourcegateway deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway -f deploy/helm/values-example.yaml \
		--set servingTLS.secretProviderClass= >/dev/null 2>&1; then echo "expected missing secret source rejection" >&2; exit 1; fi
	@if helm template sourcegateway deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway -f deploy/helm/values-example.yaml \
		--set config.credentials.gitRepository.example-github.secretName=duplicate >/dev/null 2>&1; then echo "expected duplicate credential source rejection" >&2; exit 1; fi
	@if helm template sourcegateway deploy/helm --kube-version $(KUBERNETES_VERSION) --namespace sourcegateway -f deploy/helm/values-example.yaml \
		--set config.credentials.gitRepository.example-github.secretProviderClass= >/dev/null 2>&1; then echo "expected missing credential source rejection" >&2; exit 1; fi
	@kubeconform -strict -summary -skip CustomResourceDefinition -kubernetes-version $(KUBECONFORM_SCHEMA_VERSION) \
		-schema-location 'file://$(CURDIR)/tests/kubeconform/schemas/{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json' \
		tmp/helm.yaml tmp/helm-gateway.yaml tmp/helm-ephemeral.yaml tmp/helm-secrets.yaml tmp/helm-metrics.yaml

dev: build-dev ## Run the offline development stack interactively
	@mkdir -p $(DEV_ROOT)
	@DEV_ROOT=$(DEV_ROOT) scripts/dev/prepare.sh
	@DEV_ROOT=$(DEV_ROOT) KUBEBUILDER_ASSETS="$$(go tool setup-envtest use -i -p path $(ENVTEST_VERSION))" \
		overmind start -N -f Procfile.dev

e2e: build-dev ## Run the isolated offline end-to-end test
	@ENVTEST_VERSION=$(ENVTEST_VERSION) bash scripts/e2e.sh

clean: ## Remove generated local build and test artifacts
	@rm -rf bin tmp coverage.out
