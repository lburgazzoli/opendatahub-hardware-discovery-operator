#!/usr/bin/env bash
# Shared Podman helper for the multi-platform container workflow
# (docs/development.md, Packaging). One implementation behind
# `make container-build`, `make e2e-image` and the e2e/demo workflow.
#
# Subcommands:
#   build-manifest <ref> [--base-image <image>]
#       Build the root Containerfile for exactly linux/amd64 and
#       linux/arm64 into a fresh task-owned staging manifest, validate
#       it, and promote it to <ref> in local storage. Never publishes.
#       An existing manifest list at <ref> is invalidated first (the
#       named list only, never its children); a native single-platform
#       image occupying <ref> is never deleted — the build fails
#       instead.
#   push-manifest <ref> [<destination>]
#       Validate that <ref> is a complete two-platform manifest with
#       both children available in local storage, then push every
#       member to <destination> (default: the ref itself) under a
#       bounded timeout. Native, missing-member, duplicate-platform,
#       wrong-platform and unavailable-child inputs are rejected before
#       any registry contact.
#   e2e-uuid
#       Print one fresh OS-random lowercase UUIDv4. Callers compose the
#       run-scoped ephemeral reference ttl.sh/hw-discovery-<uuidv4>:6h
#       and its separate repository/tag values from it; a combined
#       reference is never re-split on a colon.
#
# The engine is Podman and only Podman. IMAGE_ENGINE overrides the
# executable path for the offline contract tests
# (hack/scripts/test-container-workflow.sh); it never selects a
# different engine — anything not named podman is rejected.
set -euo pipefail
cd "$(dirname "$0")/../.."

die() { echo "ERROR: $*" >&2; exit 1; }

IMAGE_ENGINE=${IMAGE_ENGINE:-podman}
PLATFORMS=linux/amd64,linux/arm64
PUSH_TIMEOUT=${IMAGE_PUSH_TIMEOUT:-10m}
UUIDV4_RE='^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'

command -v jq >/dev/null 2>&1 || die "jq is required by the container workflow"
case "$(basename "$IMAGE_ENGINE")" in
podman*) ;;
*) die "only podman is supported (docker is not): engine '$IMAGE_ENGINE'" ;;
esac
command -v "$IMAGE_ENGINE" >/dev/null 2>&1 || die "container engine '$IMAGE_ENGINE' not found"

new_uuid() {
	local u
	if [[ -r /proc/sys/kernel/random/uuid ]]; then
		u=$(</proc/sys/kernel/random/uuid)
	else
		command -v uuidgen >/dev/null 2>&1 || die "no OS randomness source: /proc/sys/kernel/random/uuid is unreadable and uuidgen is not installed"
		u=$(uuidgen | tr '[:upper:]' '[:lower:]')
	fi
	[[ $u =~ $UUIDV4_RE ]] || die "generated UUID '$u' is not a canonical lowercase UUIDv4"
	printf '%s' "$u"
}

# Serialize build/push access to one reference across worktrees sharing
# Podman storage. Different references never share a lock file.
lock_ref() {
	local ref=$1
	local lockdir=${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}/hw-discovery-locks
	local lockfile
	mkdir -p "$lockdir"
	lockfile=$lockdir/$(printf '%s' "$ref" | sha256sum | cut -d' ' -f1).lock
	exec 9>"$lockfile"
	flock 9
}

# A manifest is complete when it holds exactly linux/amd64 and
# linux/arm64 once each and both children are present in local storage.
validate_manifest() {
	local ref=$1
	local json platforms digest
	# Classify locally first: `manifest exists` fails for absent refs and
	# native images without any registry contact, while `manifest
	# inspect` alone falls back to the registry for exactly those inputs
	# (breaking the reject-before-any-registry-contact contract).
	if ! "$IMAGE_ENGINE" manifest exists "$ref" >/dev/null 2>&1 ||
		! json=$("$IMAGE_ENGINE" manifest inspect "$ref" 2>/dev/null); then
		die "$ref is not a local manifest list (absent or a native single-platform image); build it first with make container-build or make e2e-image"
	fi
	platforms=$(printf '%s' "$json" | jq -r '.manifests[] | "\(.platform.os)/\(.platform.architecture)"' | sort)
	if [[ $platforms != $'linux/amd64\nlinux/arm64' ]]; then
		die "manifest $ref must contain exactly linux/amd64 and linux/arm64 (found:$(printf '%s' "$platforms" | tr '\n' ' '))"
	fi
	while read -r digest; do
		[[ -n $digest ]] || continue
		if [[ -z $("$IMAGE_ENGINE" images --filter "digest=$digest" --format '{{.ID}}') ]]; then
			die "manifest child $digest of $ref is not available in local storage; rebuild the manifest"
		fi
	done < <(printf '%s' "$json" | jq -r '.manifests[].digest')
}

cmd_build() {
	local ref=${1:-}
	local base_image=
	shift || true
	while [[ $# -gt 0 ]]; do
		case $1 in
		--base-image)
			[[ $# -ge 2 ]] || die "--base-image requires a value"
			base_image=$2
			shift 2
			;;
		--base-image=*)
			base_image=${1#*=}
			shift
			;;
		*)
			die "unknown argument: $1"
			;;
		esac
	done
	[[ -n $ref ]] || die "usage: image.sh build-manifest <ref> [--base-image <image>]"

	lock_ref "$ref"

	# Invalidate a previously built list at this ref (the named list
	# only). A native image occupying the ref is never deleted. Both
	# checks are local-only: `manifest exists` fails for native images
	# without the registry fallback `manifest inspect` would perform.
	if "$IMAGE_ENGINE" image exists "$ref" 2>/dev/null; then
		if "$IMAGE_ENGINE" manifest exists "$ref" >/dev/null 2>&1; then
			"$IMAGE_ENGINE" manifest rm "$ref" >/dev/null
		else
			die "a native single-platform image occupies $ref; refusing to overwrite it — remove it or choose another reference"
		fi
	fi

	# Not declared local: the EXIT trap runs after errexit has unwound
	# cmd_build's stack frame, where function locals are already unset.
	staging=localhost/hw-discovery-staging-$(new_uuid)
	trap '"$IMAGE_ENGINE" manifest rm "$staging" >/dev/null 2>&1 || true' EXIT

	local args=(--platform "$PLATFORMS" --manifest "$staging")
	if [[ -n $base_image ]]; then
		args+=(--build-arg "BASE_IMAGE=$base_image")
	fi
	"$IMAGE_ENGINE" build "${args[@]}" .

	validate_manifest "$staging"

	# Promote: the validated list becomes the requested reference and
	# the task-owned staging name is removed afterwards.
	"$IMAGE_ENGINE" tag "$staging" "$ref"
	"$IMAGE_ENGINE" manifest rm "$staging" >/dev/null
	trap - EXIT
}

cmd_push() {
	local ref=${1:-}
	local dest=${2:-}
	[[ -n $ref ]] || die "usage: image.sh push-manifest <ref> [<destination>]"
	[[ $# -le 2 ]] || die "usage: image.sh push-manifest <ref> [<destination>]"
	if [[ -z $dest ]]; then
		dest=$ref
	fi

	lock_ref "$ref"
	validate_manifest "$ref"
	timeout "$PUSH_TIMEOUT" "$IMAGE_ENGINE" manifest push --all "$ref" "$dest"
}

cmd_e2e_uuid() {
	# One fresh OS-random canonical UUIDv4 per invocation; callers
	# compose the run-scoped ttl.sh reference and its separate
	# repository/tag values from it (never the other way around).
	new_uuid
}

case ${1:-} in
build-manifest) shift; cmd_build "$@" ;;
push-manifest) shift; cmd_push "$@" ;;
e2e-uuid) shift; cmd_e2e_uuid "$@" ;;
*)
	die "usage: image.sh build-manifest <ref> [--base-image <image>] | push-manifest <ref> [<destination>] | e2e-uuid"
	;;
esac
