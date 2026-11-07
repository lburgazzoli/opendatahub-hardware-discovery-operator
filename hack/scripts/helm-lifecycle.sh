#!/usr/bin/env bash
# Helm release lifecycle against an explicitly named cluster. Deliberately
# inconvenient (docs/development.md, Packaging): every command requires
# KUBECONFIG and HELM_KUBECONTEXT so it can never touch an ambient
# cluster; mutating commands additionally require HELM_CHART_REVIEWED=yes
# after `make helm-review-chart`; deploy also requires whole controller
# image values (a digest reference goes in the repository, never the
# tag).
#
# usage: helm-lifecycle.sh <deploy|status|history|rollback|uninstall>
set -euo pipefail
cd "$(dirname "$0")/../.."

cmd=${1:?usage: helm-lifecycle.sh <deploy|status|history|rollback|uninstall>}

die() { echo "ERROR: $*" >&2; exit 1; }

: "${KUBECONFIG:?KUBECONFIG must explicitly name the kubeconfig file of the target cluster}"
: "${HELM_KUBECONTEXT:?HELM_KUBECONTEXT must name the exact kube context to use}"

case $cmd in
deploy | rollback | uninstall)
	test "${HELM_CHART_REVIEWED:-}" = yes ||
		die "$cmd mutates a cluster: run \`make helm-review-chart\`, inspect the chart, then retry with HELM_CHART_REVIEWED=yes"
	;;
esac

release=hardware-discovery
namespace=opendatahub-hardware-discovery-operator-system
common=(--namespace "$namespace" --kubeconfig "$KUBECONFIG" --kube-context "$HELM_KUBECONTEXT")

case $cmd in
deploy)
	repository=${CONTROLLER_IMAGE_REPOSITORY:?CONTROLLER_IMAGE_REPOSITORY must be set (whole value; digest references go here, never the tag)}
	tag=${CONTROLLER_IMAGE_TAG:?CONTROLLER_IMAGE_TAG must be set (whole value; use the chart/app version)}
	./hack/scripts/helm.sh upgrade --install "$release" dist/chart \
		"${common[@]}" --create-namespace \
		--set controllerImage.repository="$repository" \
		--set controllerImage.tag="$tag" \
		--wait --timeout 5m
	;;
status)
	./hack/scripts/helm.sh status "$release" "${common[@]}"
	;;
history)
	./hack/scripts/helm.sh history "$release" "${common[@]}"
	;;
rollback)
	./hack/scripts/helm.sh rollback "$release" "${common[@]}" --wait --timeout 5m
	;;
uninstall)
	./hack/scripts/helm.sh uninstall "$release" "${common[@]}" --wait --timeout 5m
	;;
*)
	die "unknown command: $cmd"
	;;
esac
