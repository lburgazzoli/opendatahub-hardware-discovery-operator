# Kind/simulator e2e harness and the two-plane service demonstration.
#
# Included by the root Makefile through `-include make/e2e.mk`. The include
# guard makes this fragment composable: it stays inert if it is ever pulled
# in twice (for example alongside make/helm.mk from another fragment) and
# duplicate target definitions cannot occur.
ifeq ($(strip $(HW_DISC_E2E_MK_INCLUDED)),)
HW_DISC_E2E_MK_INCLUDED := 1

# E2E_RUNTIME selects the container runtime backing the run-owned Kind
# clusters. Podman is the only supported engine: any other value fails
# before images are built or clusters are created. The suite creates its
# own run-private kubeconfigs and never touches the ambient kubeconfig.
E2E_RUNTIME ?= $(CONTAINER_TOOL)

# Image identity for standalone local `make e2e-image` builds only.
# test-e2e/demo reject any explicitly supplied value for these variables:
# their workflow owns a fresh run-scoped ttl.sh/hw-discovery-<uuid>:6h
# reference per invocation and forwards the generated values to the
# suite itself. Direct tagged-suite invocations still require explicit
# already-published whole repository/tag values (see
# hack/scripts/e2e-workflow.sh and test/support/controllerimage).
E2E_CONTROLLER_IMAGE_REPOSITORY ?= localhost/opendatahub-hardware-discovery-operator
E2E_CONTROLLER_IMAGE_TAG ?= e2e
E2E_CONTROLLER_IMAGE ?= $(E2E_CONTROLLER_IMAGE_REPOSITORY):$(E2E_CONTROLLER_IMAGE_TAG)

# E2E_TIMEOUT is the overall `go test` bound for the e2e suite. The suite
# itself bounds each profile run separately (harness verification: two
# cluster lifecycles; the two-plane demonstration: two more, each with
# the full scenario set including the incarnation race).
E2E_TIMEOUT ?= 120m

# Fails the recipe expansion — before any side effect — when one of the
# e2e image identity variables was explicitly supplied through the
# environment or the make command line. Those inputs are reserved for
# standalone build-only `e2e-image` invocations.
define reject-e2e-image-overrides
$(if $(filter environment command line,$(origin E2E_CONTROLLER_IMAGE)),$(error E2E_CONTROLLER_IMAGE is not accepted by test-e2e/demo; it is reserved for standalone e2e-image builds))
$(if $(filter environment command line,$(origin E2E_CONTROLLER_IMAGE_REPOSITORY)),$(error E2E_CONTROLLER_IMAGE_REPOSITORY is not accepted by test-e2e/demo; it is reserved for standalone e2e-image builds))
$(if $(filter environment command line,$(origin E2E_CONTROLLER_IMAGE_TAG)),$(error E2E_CONTROLLER_IMAGE_TAG is not accepted by test-e2e/demo; it is reserved for standalone e2e-image builds))
endef

.PHONY: e2e-image
e2e-image: ## Build the local linux/amd64+linux/arm64 service image (podman only; never publishes).
	$(call check-podman-only,$(E2E_RUNTIME),the e2e image build)
	hack/scripts/image.sh build-manifest $(E2E_CONTROLLER_IMAGE)$(if $(BASE_IMAGE), --base-image $(BASE_IMAGE))

.PHONY: test-e2e-compile
test-e2e-compile: ## Compile the e2e-tagged suite; needs no credentials, runtime or cluster.
	$(GO_CMD) vet -tags e2e ./test/e2e/...
	$(GO_CMD) test -tags e2e -run '^$$' -count=1 ./test/e2e/...

.PHONY: test-e2e
test-e2e: ## Run the isolated Kind e2e suite; builds and publishes a fresh run-scoped ttl.sh image first.
	$(reject-e2e-image-overrides)
	$(call check-podman-only,$(E2E_RUNTIME),the e2e workflow)
	E2E_RUNTIME=$(E2E_RUNTIME) E2E_TIMEOUT=$(E2E_TIMEOUT) GO_CMD='$(GO_CMD)'$(if $(BASE_IMAGE), BASE_IMAGE='$(BASE_IMAGE)') \
		hack/scripts/e2e-workflow.sh suite

.PHONY: demo
demo: ## Run the two-plane demonstration alone (fresh run-scoped ttl.sh image; classic and DRA profiles).
	$(reject-e2e-image-overrides)
	$(call check-podman-only,$(E2E_RUNTIME),the e2e workflow)
	E2E_RUNTIME=$(E2E_RUNTIME) E2E_TIMEOUT=$(E2E_TIMEOUT) GO_CMD='$(GO_CMD)'$(if $(BASE_IMAGE), BASE_IMAGE='$(BASE_IMAGE)') \
		hack/scripts/e2e-workflow.sh demo

endif
