# Kind/simulator e2e harness (task 010).
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

# E2E_TIMEOUT is the overall `go test` bound for the e2e suite. The suite
# itself bounds each profile run (cluster create, simulator install,
# evidence waits, fact mutation, uninstall, destroy) separately.
E2E_TIMEOUT ?= 50m

.PHONY: test-e2e-compile
test-e2e-compile: ## Compile the e2e-tagged suite; needs no credentials, runtime or cluster.
	$(GO_CMD) vet -tags e2e ./test/e2e/...
	$(GO_CMD) test -tags e2e -run '^$$' -count=1 ./test/e2e/...

.PHONY: test-e2e
test-e2e: ## Run the isolated Kind e2e suite; creates and destroys run-owned clusters.
	E2E_RUNTIME=$(E2E_RUNTIME) $(GO_CMD) test -tags e2e -count=1 -timeout $(E2E_TIMEOUT) -v ./test/e2e/...

endif
