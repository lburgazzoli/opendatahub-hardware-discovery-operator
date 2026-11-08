#!/usr/bin/env bash
# E2e/demo orchestration: one fresh public ttl.sh image per run, then the
# tagged Go suite (docs/testing.md, E2e rules).
#
# usage: e2e-workflow.sh <suite|demo>
#
# Environment:
#   E2E_RUNTIME   the Kind runtime selector; must be podman (forwarded
#                 to the suite, which enforces the same policy)
#   E2E_TIMEOUT   overall go test bound (default 120m)
#   GO_CMD        the go command to invoke (default go)
#   BASE_IMAGE    optional builder-only parent override, forwarded to
#                 the image build
#
# The workflow allocates exactly one lowercase UUIDv4 reference
# (ttl.sh/hw-discovery-<uuid>:6h), builds both architectures, validates
# and publishes the complete manifest, and only then runs the suite with
# the exact separate repository/tag values. Any earlier failure aborts
# before tests run; there is no local fallback. The generated values are
# exported explicitly so they always beat inherited environment values.
set -euo pipefail
cd "$(dirname "$0")/../.."

die() { echo "ERROR: $*" >&2; exit 1; }

mode=${1:?usage: e2e-workflow.sh <suite|demo>}
case $mode in
suite | demo) ;;
*) die "usage: e2e-workflow.sh <suite|demo>" ;;
esac

runtime=${E2E_RUNTIME:-podman}
if [[ $runtime != podman ]]; then
	die "only podman is supported as the e2e runtime (got '$runtime'); run make test-e2e / make demo with the default engine"
fi

# The workflow owns the image identity: any explicitly supplied outer
# value for the reserved e2e image variables is rejected before any
# effect. make test-e2e/demo enforce the same rule at recipe level;
# this guards direct script invocations too.
for reserved in E2E_CONTROLLER_IMAGE E2E_CONTROLLER_IMAGE_REPOSITORY E2E_CONTROLLER_IMAGE_TAG; do
	if [[ -n ${!reserved+x} ]]; then
		die "$reserved is not accepted by the e2e workflow; it is reserved for standalone local e2e-image builds"
	fi
done

image_sh=$(dirname "$0")/image.sh

# One reference per run, reused unchanged through build, publish and
# test; never reallocated or taken from the environment. The separate
# repository/tag values are composed from the UUID, never re-split
# from a combined reference.
uuid=$("$image_sh" e2e-uuid)
repository=ttl.sh/hw-discovery-$uuid
tag=6h
ref=$repository:$tag

echo "e2e image reference: $ref" >&2

build_args=()
if [[ -n ${BASE_IMAGE:-} ]]; then
	build_args+=(--base-image "$BASE_IMAGE")
fi
"$image_sh" build-manifest "$ref" ${build_args[@]+"${build_args[@]}"}
"$image_sh" push-manifest "$ref"

go_cmd=${GO_CMD:-go}
e2e_timeout=${E2E_TIMEOUT:-120m}

export E2E_RUNTIME=$runtime
export E2E_CONTROLLER_IMAGE_REPOSITORY=$repository
export E2E_CONTROLLER_IMAGE_TAG=$tag

if [[ $mode == demo ]]; then
	exec "$go_cmd" test -tags e2e -count=1 -timeout "$e2e_timeout" -v -run '^TestHardwareDiscoveryDemonstration$' ./test/e2e/...
fi
exec "$go_cmd" test -tags e2e -count=1 -timeout "$e2e_timeout" -v ./test/e2e/...
