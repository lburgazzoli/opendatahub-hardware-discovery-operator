# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# CONTAINER_TOOL defines the container tool to be used for building images.
# The local development and Kind harness baseline is rootless podman; Docker
# is an accepted override (CONTAINER_TOOL=docker).
CONTAINER_TOOL ?= podman

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Tool invocation style: pinned `go run module@version` only. Never @latest,
# unpinned curl installers, or hidden local installations. Versions below are
# the approved pins from the task-001 execution contract
# (docs/design/poc-contract.md §2.2).
GO_CMD ?= go
CONTROLLER_TOOLS_VERSION ?= v0.22.0
KUSTOMIZE_VERSION ?= v5.8.1
GOLANGCI_LINT_VERSION ?= v2.14.0
CONTROLLER_GEN = $(GO_CMD) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
KUSTOMIZE = $(GO_CMD) run sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)
GOLANGCI_LINT = $(GO_CMD) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Unit tests always run with the race detector.
GO_TEST_FLAGS ?= -race

.DEFAULT_GOAL := help

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of
# the file as xyz: ## something, and pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info about the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info about the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# NOTE on generated artifacts: config/crd/bases/, config/rbac/role.yaml and
# config/manager/kustomization.yaml are gitignored on purpose. A clean clone
# reproduces them with `make manifests` (CRDs and RBAC from markers) and with
# `make build-installer` (the manager kustomization is recreated
# by `kustomize create --autodetect` when absent, then `kustomize edit` sets
# the image). api/**/zz_generated.deepcopy.go is tracked and drift-checked by
# `make verify`.

.PHONY: manifests
manifests: ## Generate ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd paths="./api/...;./cmd/...;./internal/..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./api/...;./cmd/...;./internal/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	$(GO_CMD) fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	$(GO_CMD) vet ./...

.PHONY: lint
lint: ## Run golangci-lint linter.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint linter and perform fixes.
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: ## Verify golangci-lint linter configuration.
	$(GOLANGCI_LINT) config verify

.PHONY: test
test: manifests generate ## Run unit tests (race detector enabled; no cluster, no manager).
	$(GO_CMD) test $(GO_TEST_FLAGS) ./...

.PHONY: verify
verify: manifests generate fmt vet lint test drift-check ## Run all cluster-free checks: generation, formatting, lint, tests and drift.

.PHONY: drift-check
drift-check: ## Fail if tracked files drifted from generated/formatted state.
	@git diff --exit-code -- . || { \
		echo "ERROR: working tree differs from committed state after manifests/generate/fmt." >&2; \
		echo "       Regenerate (make manifests generate fmt) and commit the result." >&2; \
		exit 1; }

##@ Build

.PHONY: build
build: ## Build manager binary.
	$(GO_CMD) build -o bin/manager ./cmd

.PHONY: run
run: manifests generate fmt vet ## Run the operator subcommand from your host.
	$(GO_CMD) run ./cmd operator

.PHONY: container-build
container-build: ## Build the manager container image (podman by default, CONTAINER_TOOL=docker to override).
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t $(IMG) .

.PHONY: container-push
container-push: ## Push the manager container image.
	$(CONTAINER_TOOL) push $(IMG)

.PHONY: build-installer
build-installer: manifests generate ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && { [ -f kustomization.yaml ] || $(KUSTOMIZE) create --autodetect; } && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default > dist/install.yaml

# NOTE on deployment: there are deliberately no cluster-mutating
# `install`/`deploy`/`undeploy` targets. Deployment ownership belongs to
# the Helm chart (task 009) and the Kind harness (task 010), which
# require an explicit kubeconfig/context; build-installer only renders
# YAML into dist/ and never contacts a cluster.

# Task-owned make fragments. make/helm.mk is owned by the Helm packaging task
# and make/e2e.mk by the Kind harness task; they are included silently when
# present and must stay optional so build/test/verify never require them.
-include make/helm.mk
-include make/e2e.mk
