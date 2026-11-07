# Kind/simulator e2e harness and the two-plane service demonstration
# (tasks 010 and 011).
#
# Included by the root Makefile through `-include make/e2e.mk`. The include
# guard makes this fragment composable: it stays inert if it is ever pulled
# in twice (for example alongside make/helm.mk from another fragment) and
# duplicate target definitions cannot occur.
ifeq ($(strip $(HW_DISC_E2E_MK_INCLUDED)),)
HW_DISC_E2E_MK_INCLUDED := 1

# E2E_RUNTIME selects the container runtime backing the run-owned Kind
# clusters: podman by default, docker as the accepted override. It follows
# CONTAINER_TOOL and is passed down to the e2e suite, which creates its own
# run-private kubeconfigs and never touches the ambient kubeconfig.
E2E_RUNTIME ?= $(CONTAINER_TOOL)

# The service image the demonstration installs from the reviewed chart.
# It is built locally and loaded into each run-owned cluster as cargo —
# no registry push or pull is ever involved. The repository/tag pair is
# passed to the suite, which fails deterministically (never skips) when
# the image is missing from the runtime.
E2E_CONTROLLER_IMAGE_REPOSITORY ?= localhost/opendatahub-hardware-discovery-operator
E2E_CONTROLLER_IMAGE_TAG ?= e2e
E2E_CONTROLLER_IMAGE ?= $(E2E_CONTROLLER_IMAGE_REPOSITORY):$(E2E_CONTROLLER_IMAGE_TAG)

# E2E_TIMEOUT is the overall `go test` bound for the e2e suite. The suite
# itself bounds each profile run separately (harness verification: two
# cluster lifecycles; the two-plane demonstration: two more, each with
# the full scenario set including the incarnation race).
E2E_TIMEOUT ?= 120m

.PHONY: e2e-image
e2e-image: ## Build the service image the e2e demonstration installs (podman by default).
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t $(E2E_CONTROLLER_IMAGE) .

.PHONY: test-e2e-compile
test-e2e-compile: ## Compile the e2e-tagged suite; needs no credentials, runtime or cluster.
	$(GO_CMD) vet -tags e2e ./test/e2e/...
	$(GO_CMD) test -tags e2e -run '^$$' -count=1 ./test/e2e/...

.PHONY: test-e2e
test-e2e: e2e-image ## Run the isolated Kind e2e suite (harness verification + two-plane demonstration).
	E2E_RUNTIME=$(E2E_RUNTIME) \
	E2E_CONTROLLER_IMAGE_REPOSITORY=$(E2E_CONTROLLER_IMAGE_REPOSITORY) \
	E2E_CONTROLLER_IMAGE_TAG=$(E2E_CONTROLLER_IMAGE_TAG) \
	$(GO_CMD) test -tags e2e -count=1 -timeout $(E2E_TIMEOUT) -v ./test/e2e/...

.PHONY: demo
demo: e2e-image ## Run the two-plane demonstration alone (classic and DRA profiles, no harness re-verification).
	E2E_RUNTIME=$(E2E_RUNTIME) \
	E2E_CONTROLLER_IMAGE_REPOSITORY=$(E2E_CONTROLLER_IMAGE_REPOSITORY) \
	E2E_CONTROLLER_IMAGE_TAG=$(E2E_CONTROLLER_IMAGE_TAG) \
	$(GO_CMD) test -tags e2e -count=1 -timeout $(E2E_TIMEOUT) -v -run '^TestHardwareDiscoveryDemonstration$$' ./test/e2e/...

endif
