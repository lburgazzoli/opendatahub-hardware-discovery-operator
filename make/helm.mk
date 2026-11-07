# Helm packaging for the hardware discovery service (task 009).
#
# Pipeline (docs/development.md, "Helm contract shared by runtime and
# packaging"):
#   source config -> make manifests generate build
#                 -> make helm-chart [IMG=...]
#                 -> helm-review-chart / helm-lint / helm-render /
#                    helm-package / module-lint
#
# This file stays declarative — fixed names, pinned tools, and the shell
# logic in hack/scripts. Everything is cluster-free except the lifecycle
# targets, which fail closed without an explicit KUBECONFIG and
# HELM_KUBECONTEXT (mutating ones additionally require
# HELM_CHART_REVIEWED=yes).

KUBEBUILDER_VERSION ?= v4.16.0
KUBEBUILDER = $(GO_CMD) run sigs.k8s.io/kubebuilder/v4@$(KUBEBUILDER_VERSION)

# Pinned Helm CLI; hack/scripts/helm.sh owns the version pin.
HELM = ./hack/scripts/helm.sh

# Release identity. The chart pins fullnameOverride, so rendered object
# names are independent of the release name.
HELM_RELEASE = hardware-discovery
HELM_NAMESPACE = opendatahub-hardware-discovery-operator-system

##@ Helm packaging (cluster-free)

.PHONY: helm-chart
helm-chart: ## Regenerate dist/chart via the pinned Kubebuilder helm plugin, then curate it.
	$(KUBEBUILDER) edit --plugins=helm/v2-alpha
	hack/scripts/curate-chart.sh

.PHONY: helm-review-chart
helm-review-chart: helm-chart ## Regenerate the chart and require byte-identity with the committed reviewed state.
	@status=$$(git status --porcelain -- dist/chart); if [ -n "$$status" ]; then \
		echo "ERROR: dist/chart differs from the committed reviewed chart:" >&2; \
		echo "$$status" >&2; \
		echo "       Review the diff (git diff -- dist/chart), then commit it." >&2; \
		exit 1; \
	fi
	@echo "Reviewed chart inventory:"
	@find dist/chart -type f | sort

.PHONY: helm-lint
helm-lint: ## Lint the chart with the pinned Helm CLI.
	$(HELM) lint dist/chart

.PHONY: helm-render
helm-render: ## Render the chart with default values into dist/rendered.yaml.
	mkdir -p dist
	$(HELM) template $(HELM_RELEASE) dist/chart --namespace $(HELM_NAMESPACE) > dist/rendered.yaml
	@echo "Rendered chart written to dist/rendered.yaml"

.PHONY: helm-package
helm-package: ## Package the chart into dist/ and verify the archive inventory and usability.
	hack/scripts/package-chart.sh

.PHONY: module-lint
module-lint: helm-chart ## Lint packaging/module.yaml against the chart with the pinned odh-platform-tools (offline).
	hack/scripts/module-lint.sh

##@ Helm lifecycle (require an explicit kubeconfig and context)

.PHONY: helm-deploy
helm-deploy: ## Install/upgrade the release (mutates a cluster). Requires KUBECONFIG, HELM_KUBECONTEXT, HELM_CHART_REVIEWED=yes and whole controllerImage values.
	hack/scripts/helm-lifecycle.sh deploy

.PHONY: helm-status
helm-status: ## Show release status (read-only). Requires KUBECONFIG and HELM_KUBECONTEXT.
	hack/scripts/helm-lifecycle.sh status

.PHONY: helm-history
helm-history: ## Show release history (read-only). Requires KUBECONFIG and HELM_KUBECONTEXT.
	hack/scripts/helm-lifecycle.sh history

.PHONY: helm-rollback
helm-rollback: ## Roll back the release (mutates a cluster). Requires KUBECONFIG, HELM_KUBECONTEXT and HELM_CHART_REVIEWED=yes.
	hack/scripts/helm-lifecycle.sh rollback

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the release, retaining the CRDs (mutates a cluster). Requires KUBECONFIG, HELM_KUBECONTEXT and HELM_CHART_REVIEWED=yes.
	hack/scripts/helm-lifecycle.sh uninstall
