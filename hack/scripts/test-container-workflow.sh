#!/usr/bin/env bash
# Offline contract tests for the container workflow: make container-build,
# container-push, e2e-image, test-e2e and demo run against two
# argument-recording stubs (podman, go) backed by a small stateful fake
# of the exact podman surface used by hack/scripts/image.sh. No container
# engine, registry, network or cluster is contacted, and no repository
# file is written: all state lives in a mktemp directory removed on exit.
#
# Covered contract (docs/testing.md):
#   - exactly linux/amd64+linux/arm64 builds, BASE_IMAGE forwarding
#   - all-member push, invalid-list rejection, no push on build-only paths
#   - non-podman and outer e2e image override rejection before effects
#   - exact env/ref propagation, UUIDv4 grammar, one ID per run,
#     distinct concurrent-run IDs, fail-closed propagation
#   - local-only classification of invalid inputs (no registry contact)
#   - failed rebuilds invalidate any list state at the ref, including
#     native-childless lists that `image exists` cannot see
#   - alias reference spellings share one per-ref lock
#   - side-effect-free help and dry runs
#
# shellcheck disable=SC2016
# (assertion snippets are single-quoted on purpose: check() evaluates
# them later against per-case state)
set -euo pipefail
cd "$(dirname "$0")/../.."

command -v jq >/dev/null 2>&1 || { echo "ERROR: jq is required by hack/scripts/image.sh" >&2; exit 1; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/hw-discovery-workflow-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
STUBS=$WORK/stubs
mkdir -p "$STUBS"

pass=0
fail=0
ok() { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# --------------------------------------------------------------------------
# Stubs. Written with quoted heredocs so nothing expands at write time;
# they read STUB_LOG/STUB_STATE and their scenario knobs from the
# environment at run time.
# --------------------------------------------------------------------------

cat >"$STUBS/podman" <<'STUB'
#!/usr/bin/env bash
# Argument-recording podman stub: a stateful fake of the podman surface
# used by hack/scripts/image.sh.
set -u
log=${STUB_LOG:?}
state=${STUB_STATE:?}
printf 'PODMAN %s\n' "$*" >>"$log"

hash_ref() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }

cmd=${1:-}
shift || true
case $cmd in
build)
	platforms= manifest=
	while [[ $# -gt 0 ]]; do
		case $1 in
		--platform) platforms=$2; shift 2 ;;
		--manifest) manifest=$2; shift 2 ;;
		*) shift ;;
		esac
	done
	# A working engine records exactly the requested platform set;
	# STUB_BUILD_PLATFORMS lets a case simulate a broken engine.
	platforms=${STUB_BUILD_PLATFORMS:-$platforms}
	rc=${STUB_BUILD_RC:-0}
	if [[ $rc == 0 ]]; then
		mkdir -p "$state/refs"
		f=$state/refs/$(hash_ref "$manifest")
		: >"$f"
		IFS=, read -ra parts <<<"$platforms"
		for p in "${parts[@]}"; do
			printf '%s sha256:%s\n' "$p" "$(printf '%s/%s' "$manifest" "$p" | sha256sum | cut -d' ' -f1)" >>"$f"
		done
	fi
	exit "$rc"
	;;
manifest)
	sub=${1:-}
	shift || true
	case $sub in
	exists)
		# Local-only classification, mirroring podman semantics
		# (verified against podman 6.1.3): 0 for a local manifest
		# list, 125 for a native image at the ref, 1 for an absent
		# ref — and never any registry contact.
		if [[ -f $state/refs/$(hash_ref "${1:-}") ]]; then exit 0; fi
		if [[ -f $state/native/$(hash_ref "${1:-}") ]]; then
			echo "stub podman: ${1:-}: image is not a manifest list" >&2
			exit 125
		fi
		exit 1
		;;
	inspect)
		f=$state/refs/$(hash_ref "${1:-}")
		[[ -f $f ]] || exit 1
		printf '{\n  "schemaVersion": 2,\n  "manifests": [\n'
		sep=''
		while read -r p d; do
			printf '%s    {"mediaType": "application/vnd.oci.image.manifest.v1+json", "size": 923, "digest": "%s", "platform": {"architecture": "%s", "os": "%s"}}\n' \
				"$sep" "$d" "${p#*/}" "${p%/*}"
			sep=','
		done <"$f"
		printf '  ]\n}\n'
		;;
	rm)
		rm -f "$state/refs/$(hash_ref "${1:-}")"
		;;
	push)
		exit "${STUB_PUSH_RC:-0}"
		;;
	esac
	;;
tag)
	src=${1:-} dst=${2:-}
	mkdir -p "$state/refs" "$state/native"
	if [[ -f $state/refs/$(hash_ref "$src") ]]; then
		cp "$state/refs/$(hash_ref "$src")" "$state/refs/$(hash_ref "$dst")"
	elif [[ -f $state/native/$(hash_ref "$src") ]]; then
		: >"$state/native/$(hash_ref "$dst")"
	else
		echo "stub podman: tag: $src not known" >&2
		exit 1
	fi
	;;
image)
	case ${1:-} in
	exists)
		# Faithful to podman 6.1.3: `image exists` resolves a
		# manifest list through its native-platform child, so
		# empty and foreign-only lists are invisible to it.
		[[ -f $state/native/$(hash_ref "${2:-}") ]] && exit 0
		f=$state/refs/$(hash_ref "${2:-}")
		[[ -f $f ]] && grep -q '^linux/amd64 ' "$f" && exit 0
		exit 1
		;;
	esac
	exit 1
	;;
images)
	digest=
	while [[ $# -gt 0 ]]; do
		case $1 in
		--filter)
			case $2 in digest=*) digest=${2#digest=} ;; esac
			shift 2
			;;
		*) shift ;;
		esac
	done
	[[ -n $digest ]] || exit 0
	[[ -f $state/unavailable/$digest ]] && exit 0
	if grep -rqF -- "$digest" "$state/refs" 2>/dev/null; then
		printf '%s\n' "${digest:7:12}"
	fi
	exit 0
	;;
esac
exit 0
STUB

cat >"$STUBS/go" <<'STUB'
#!/usr/bin/env bash
# Argument-recording go stub: captures the e2e test invocation and the
# image identity environment the workflow forwards to the suite.
set -u
log=${STUB_LOG:?}
printf 'GO %s\n' "$*" >>"$log"
printf 'GOENV E2E_RUNTIME=%s E2E_CONTROLLER_IMAGE_REPOSITORY=%s E2E_CONTROLLER_IMAGE_TAG=%s\n' \
	"${E2E_RUNTIME:-}" "${E2E_CONTROLLER_IMAGE_REPOSITORY:-}" "${E2E_CONTROLLER_IMAGE_TAG:-}" >>"$log"
exit "${STUB_GO_RC:-0}"
STUB

chmod +x "$STUBS/podman" "$STUBS/go"

# --------------------------------------------------------------------------
# Harness
# --------------------------------------------------------------------------

CASE_N=0
CASE_DIR=
begin_case() {
	CASE_N=$((CASE_N + 1))
	CASE_DIR=$WORK/case-$CASE_N
	mkdir -p "$CASE_DIR/state" "$CASE_DIR/run"
	: >"$CASE_DIR/events.log"
	printf '\n== case %d: %s\n' "$CASE_N" "$1"
}

# with_stubs runs one command under the current case's stub environment.
# Every stubbed make/script invocation must go through here. IMAGE_ENGINE
# and GO_CMD are pinned to the stub executables explicitly: inherited
# absolute selectors bypass a PATH stub, and the Makefile re-exports
# GO_CMD from make's own environment, so PATH alone is not hermetic.
# MAKEFLAGS and MAKEFILES are stripped: an outer make's command-line
# assignments (for example `make GO_CMD=/usr/bin/go
# test-container-workflow`) propagate to nested makes through MAKEFLAGS
# as command-line variables and would beat these environment-level pins.
with_stubs() {
	env -u MAKEFLAGS -u MAKEFILES PATH="$STUBS:$PATH" XDG_RUNTIME_DIR="$CASE_DIR/run" \
		IMAGE_ENGINE="$STUBS/podman" GO_CMD="$STUBS/go" \
		STUB_LOG="$CASE_DIR/events.log" STUB_STATE="$CASE_DIR/state" "$@"
}

expect_make() { # desc, make args... — success expected
	local desc=$1
	shift
	if with_stubs make "$@" >"$CASE_DIR/make.out" 2>&1; then
		ok "$desc"
	else
		bad "$desc (make exited non-zero)"
		sed -n '1,8p' "$CASE_DIR/make.out" >&2
	fi
}

expect_make_fail() { # desc, make args... — failure expected
	local desc=$1
	shift
	if with_stubs make "$@" >"$CASE_DIR/make.out" 2>&1; then
		bad "$desc (make unexpectedly succeeded)"
	else
		ok "$desc"
	fi
}

expect_cmd_fail() { # desc, cmd... — failure expected
	local desc=$1
	shift
	if with_stubs "$@" >"$CASE_DIR/cmd.out" 2>&1; then
		bad "$desc (unexpectedly succeeded)"
	else
		ok "$desc"
	fi
}

expect_log() { # desc, extended regex expected in the event log
	if grep -Eq -- "$2" "$CASE_DIR/events.log"; then ok "$1"; else bad "$1"; fi
}
expect_no_log() { # desc, extended regex that must not appear in the event log
	if grep -Eq -- "$2" "$CASE_DIR/events.log"; then bad "$1"; else ok "$1"; fi
}
expect_out() { # desc, extended regex expected in the last make output
	if grep -Eq -- "$2" "$CASE_DIR/make.out"; then ok "$1"; else bad "$1"; fi
}
expect_no_out() { # desc, extended regex that must not appear in the last make output
	if grep -Eq -- "$2" "$CASE_DIR/make.out"; then bad "$1"; else ok "$1"; fi
}
check() { # desc, shell snippet evaluated with the harness functions
	if eval "$2"; then ok "$1"; else bad "$1"; fi
}

hash_ref() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }
ref_file() { printf '%s/refs/%s' "$CASE_DIR/state" "$(hash_ref "$1")"; }
platforms_of_ref() { cut -d' ' -f1 "$(ref_file "$1")" 2>/dev/null | sort | tr '\n' ' ' | sed 's/ $//'; }

seed_manifest() { # ref, "os/arch digest" entries...
	local ref=$1 f entry
	shift
	mkdir -p "$CASE_DIR/state/refs"
	f=$(ref_file "$ref")
	: >"$f"
	for entry in "$@"; do printf '%s\n' "$entry" >>"$f"; done
}
seed_native() { # ref
	mkdir -p "$CASE_DIR/state/native"
	: >"$CASE_DIR/state/native/$(hash_ref "$1")"
}
hide_child() { # digest
	mkdir -p "$CASE_DIR/state/unavailable"
	: >"$CASE_DIR/state/unavailable/$1"
}

UUID_RE='[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}'
REF_RE="ttl\\.sh/hw-discovery-$UUID_RE:6h"
D1=sha256:$(printf 'workflow-d1' | sha256sum | cut -d' ' -f1)
D2=sha256:$(printf 'workflow-d2' | sha256sum | cut -d' ' -f1)

# --------------------------------------------------------------------------
# Cases
# --------------------------------------------------------------------------

begin_case 'container-build builds exactly two platforms and never pushes'
expect_make 'make container-build succeeds' container-build IMG=localhost/t1:1
expect_log 'build requests exactly linux/amd64,linux/arm64' \
	"^PODMAN build --platform linux/amd64,linux/arm64 --manifest localhost/hw-discovery-staging-$UUID_RE "
expect_log 'the validated staging manifest is promoted to the requested ref' \
	"^PODMAN tag localhost/hw-discovery-staging-$UUID_RE localhost/t1:1$"
expect_log 'the task-owned staging name is removed afterwards' \
	"^PODMAN manifest rm localhost/hw-discovery-staging-$UUID_RE$"
expect_no_log 'the build-only path never pushes' 'manifest push'
check 'the final manifest holds exactly the two platforms' \
	'[[ $(platforms_of_ref localhost/t1:1) == "linux/amd64 linux/arm64" ]]'

begin_case 'container-build forwards a command-line BASE_IMAGE override'
expect_make 'make container-build with BASE_IMAGE succeeds' \
	container-build IMG=localhost/t2:1 BASE_IMAGE=registry.access.redhat.com/ubi10/go-toolset:1.26
expect_log 'BASE_IMAGE reaches the engine build' \
	'--build-arg BASE_IMAGE=registry\.access\.redhat\.com/ubi10/go-toolset:1\.26'

begin_case 'container-build forwards an environment BASE_IMAGE override'
BASE_IMAGE=registry.access.redhat.com/ubi10/go-toolset:1.26
export BASE_IMAGE
expect_make 'make container-build with an inherited BASE_IMAGE succeeds' container-build IMG=localhost/t2b:1
unset BASE_IMAGE
expect_log 'the inherited BASE_IMAGE reaches the engine build' \
	'--build-arg BASE_IMAGE=registry\.access\.redhat\.com/ubi10/go-toolset:1\.26'

begin_case 'rebuilding a manifest invalidates only the named list'
expect_make 'first build succeeds' container-build IMG=localhost/t3:1
expect_make 'second build succeeds' container-build IMG=localhost/t3:1
expect_log 'the existing list is invalidated before the rebuild' \
	"^PODMAN manifest rm localhost/t3:1$"
check 'no duplicate platform entries accumulate' \
	'[[ $(platforms_of_ref localhost/t3:1) == "linux/amd64 linux/arm64" ]]'
check 'only the named list and task-owned staging names are ever removed' '
	rm_bad=0
	while read -r _ _ _ target; do
		case $target in
		localhost/t3:1) ;;
		localhost/hw-discovery-staging-*) ;;
		*) rm_bad=1 ;;
		esac
	done < <(grep "^PODMAN manifest rm " "$CASE_DIR/events.log")
	[[ $rm_bad == 0 ]]'

begin_case 'a native image occupying the ref is refused, not deleted'
seed_native localhost/t4:1
expect_make_fail 'make container-build fails' container-build IMG=localhost/t4:1
expect_no_log 'no engine build runs' '^PODMAN build '
expect_no_log 'the native image is not removed' 'manifest rm localhost/t4:1'
check 'the native image still occupies the ref' \
	'[[ -e $CASE_DIR/state/native/$(hash_ref localhost/t4:1) ]]'

begin_case 'e2e-image builds both platforms locally and never publishes'
expect_make 'make e2e-image succeeds' e2e-image
expect_log 'both platforms are built' \
	"^PODMAN build --platform linux/amd64,linux/arm64 --manifest localhost/hw-discovery-staging-$UUID_RE "
expect_no_log 'e2e-image never pushes' 'manifest push'
check 'the local e2e image default holds both platforms' \
	'[[ $(platforms_of_ref localhost/opendatahub-hardware-discovery-operator:e2e) == "linux/amd64 linux/arm64" ]]'

begin_case 'non-podman selectors are rejected before any effect'
expect_make_fail 'container-build rejects docker' container-build CONTAINER_TOOL=docker
expect_make_fail 'container-push rejects docker' container-push CONTAINER_TOOL=docker
expect_make_fail 'e2e-image rejects a docker runtime' e2e-image E2E_RUNTIME=docker
expect_make_fail 'test-e2e rejects a docker runtime' test-e2e E2E_RUNTIME=docker
expect_make_fail 'demo rejects a docker runtime' demo E2E_RUNTIME=docker
expect_cmd_fail 'image.sh rejects a non-podman engine' \
	env IMAGE_ENGINE=docker hack/scripts/image.sh build-manifest localhost/x:1
check 'no stubbed engine command ran' '[[ ! -s $CASE_DIR/events.log ]]'

begin_case 'outer e2e image overrides are rejected before any effect'
expect_make_fail 'test-e2e rejects E2E_CONTROLLER_IMAGE' test-e2e E2E_CONTROLLER_IMAGE=ttl.sh/x:6h
expect_make_fail 'test-e2e rejects E2E_CONTROLLER_IMAGE_REPOSITORY' test-e2e E2E_CONTROLLER_IMAGE_REPOSITORY=ttl.sh/x
expect_make_fail 'test-e2e rejects E2E_CONTROLLER_IMAGE_TAG' test-e2e E2E_CONTROLLER_IMAGE_TAG=6h
expect_make_fail 'demo rejects E2E_CONTROLLER_IMAGE' demo E2E_CONTROLLER_IMAGE=ttl.sh/x:6h
E2E_CONTROLLER_IMAGE_REPOSITORY=ttl.sh/x
export E2E_CONTROLLER_IMAGE_REPOSITORY
expect_make_fail 'test-e2e rejects an environment override' test-e2e
unset E2E_CONTROLLER_IMAGE_REPOSITORY
expect_cmd_fail 'e2e-workflow.sh rejects direct environment overrides' \
	env E2E_CONTROLLER_IMAGE_TAG=6h hack/scripts/e2e-workflow.sh suite
check 'no stubbed engine or test command ran' '[[ ! -s $CASE_DIR/events.log ]]'

begin_case 'test-e2e runs one run-scoped ttl.sh reference through build, publish and test'
expect_make 'make test-e2e succeeds' test-e2e
expect_log "the reference uses the canonical UUIDv4 grammar" "$REF_RE"
expect_log 'both platforms are built into a fresh staging manifest' \
	"^PODMAN build --platform linux/amd64,linux/arm64 --manifest localhost/hw-discovery-staging-$UUID_RE "
expect_log 'staging is promoted to the run reference' \
	"^PODMAN tag localhost/hw-discovery-staging-$UUID_RE $REF_RE\$"
expect_log 'every member is published' "^PODMAN manifest push --all $REF_RE $REF_RE\$"
expect_log 'the suite receives the exact separate repository and tag values' \
	"^GOENV E2E_RUNTIME=podman E2E_CONTROLLER_IMAGE_REPOSITORY=ttl\.sh/hw-discovery-$UUID_RE E2E_CONTROLLER_IMAGE_TAG=6h\$"
expect_log 'the full tagged suite runs' \
	'^GO test -tags e2e -count=1 -timeout 120m -v \./test/e2e/\.\.\.$'
check 'exactly one reference is allocated per run' \
	'[[ $(grep -oE "ttl\.sh/hw-discovery-[0-9a-f-]{36}" "$CASE_DIR/events.log" | sort -u | wc -l) == 1 ]]'
check 'build, publish and test run in order' '
	b=$(grep -n "^PODMAN build " "$CASE_DIR/events.log" | head -1 | cut -d: -f1)
	p=$(grep -n "manifest push" "$CASE_DIR/events.log" | head -1 | cut -d: -f1)
	t=$(grep -n "^GO test" "$CASE_DIR/events.log" | head -1 | cut -d: -f1)
	[[ -n $b && -n $p && -n $t && $b -lt $p && $p -lt $t ]]'

begin_case 'demo runs the discovery selection through the same workflow'
expect_make 'make demo succeeds' demo
expect_log 'demo publishes the run reference' "^PODMAN manifest push --all $REF_RE $REF_RE\$"
expect_log 'demo runs only the demonstration test' \
	'^GO test -tags e2e -count=1 -timeout 120m -v -run \^TestHardwareDiscoveryDemonstration\$ \./test/e2e/\.\.\.$'

begin_case 'concurrent e2e runs use distinct run-scoped references'
pids=()
for i in 1 2; do
	d=$CASE_DIR/run$i
	mkdir -p "$d/state"
	: >"$d/events.log"
	# The second run inherits hostile absolute selectors after the first
	# has already forked with a clean environment: the explicit pins
	# below must still route it to the stubs. This invocation mirrors
	# with_stubs manually because the log and state paths are per run
	# here rather than per case.
	if [[ $i == 2 ]]; then
		export IMAGE_ENGINE=/usr/bin/podman GO_CMD=/usr/bin/go
	fi
	env -u MAKEFLAGS -u MAKEFILES PATH="$STUBS:$PATH" XDG_RUNTIME_DIR="$CASE_DIR/run" \
		IMAGE_ENGINE="$STUBS/podman" GO_CMD="$STUBS/go" \
		STUB_LOG="$d/events.log" STUB_STATE="$d/state" \
		make test-e2e >"$d/make.out" 2>&1 &
	pids+=($!)
done
unset IMAGE_ENGINE GO_CMD
if wait "${pids[0]}"; then ok 'first concurrent run succeeds'; else bad 'first concurrent run failed'; fi
if wait "${pids[1]}"; then ok 'second concurrent run succeeds'; else bad 'second concurrent run failed'; fi
check 'the hostile-selector run still used the stubs' \
	'[[ $(grep -c "^PODMAN build " "$CASE_DIR/run2/events.log") -ge 1 && $(grep -c "^GO test " "$CASE_DIR/run2/events.log") -ge 1 ]]'
# r1/r2 are consumed by the check snippets evaluated below
# shellcheck disable=SC2034
r1=$(grep -oE "ttl\.sh/hw-discovery-[0-9a-f-]{36}:6h" "$CASE_DIR/run1/events.log" | sort -u)
# shellcheck disable=SC2034
r2=$(grep -oE "ttl\.sh/hw-discovery-[0-9a-f-]{36}:6h" "$CASE_DIR/run2/events.log" | sort -u)
check 'each run uses exactly one reference' '
	[[ $(grep -oE "ttl\.sh/hw-discovery-[0-9a-f-]{36}:6h" "$CASE_DIR/run1/events.log" | sort -u | wc -l) == 1 &&
		$(grep -oE "ttl\.sh/hw-discovery-[0-9a-f-]{36}:6h" "$CASE_DIR/run2/events.log" | sort -u | wc -l) == 1 ]]'
check 'the two runs use different references' '[[ -n $r1 && -n $r2 && $r1 != $r2 ]]'

begin_case 'a failed build fails the workflow closed before publish or test'
STUB_BUILD_RC=1
export STUB_BUILD_RC
expect_make_fail 'make test-e2e fails' test-e2e
unset STUB_BUILD_RC
expect_log 'the task-owned staging state is removed even when the build fails' \
	"^PODMAN manifest rm localhost/hw-discovery-staging-$UUID_RE$"
expect_no_log 'nothing is published' 'manifest push'
expect_no_log 'no test runs' '^(GO |GOENV)'

begin_case 'a failed publish fails the workflow closed before tests'
STUB_PUSH_RC=1
export STUB_PUSH_RC
expect_make_fail 'make test-e2e fails' test-e2e
unset STUB_PUSH_RC
expect_log 'the publish was attempted' 'manifest push'
expect_no_log 'no test runs after a failed publish' '^(GO |GOENV)'

begin_case 'a failed test run fails make test-e2e'
STUB_GO_RC=1
export STUB_GO_RC
expect_make_fail 'make test-e2e fails' test-e2e
unset STUB_GO_RC
expect_log 'the test ran' '^GO test '

begin_case 'container-push publishes every member of a complete manifest'
expect_make 'build succeeds' container-build IMG=localhost/t14:1
expect_make 'push succeeds' container-push IMG=localhost/t14:1
expect_log 'the push is an explicit all-member operation' \
	"^PODMAN manifest push --all localhost/t14:1 localhost/t14:1$"

begin_case 'container-push rejects incomplete or invalid inputs before any push'
seed_native localhost/t15a:1
expect_make_fail 'a native image input is rejected' container-push IMG=localhost/t15a:1
seed_manifest localhost/t15b:1 "linux/amd64 $D1" "linux/ppc64le $D2"
expect_make_fail 'a wrong platform set is rejected' container-push IMG=localhost/t15b:1
seed_manifest localhost/t15c:1 "linux/amd64 $D1"
expect_make_fail 'a missing member is rejected' container-push IMG=localhost/t15c:1
seed_manifest localhost/t15d:1 "linux/amd64 $D1" "linux/amd64 $D2"
expect_make_fail 'a duplicate platform is rejected' container-push IMG=localhost/t15d:1
seed_manifest localhost/t15e:1 "linux/amd64 $D1" "linux/arm64 $D2"
hide_child "$D2"
expect_make_fail 'an unavailable child is rejected' container-push IMG=localhost/t15e:1
expect_no_log 'no registry push happened for any invalid input' 'manifest push'

begin_case 'invalid inputs are classified locally, without registry inspection'
seed_native localhost/t16a:1
expect_make_fail 'push rejects a native ref' container-push IMG=localhost/t16a:1
expect_make_fail 'push rejects an absent ref' container-push IMG=localhost/t16c:1
seed_native localhost/t16b:1
expect_make_fail 'build refuses a native occupant' container-build IMG=localhost/t16b:1
expect_log 'the local-only list check ran' 'manifest exists localhost/t16[abc]:1'
expect_no_log 'no manifest inspect ran for the invalid inputs' 'manifest inspect'

begin_case 'the offline suite is hermetic against inherited engine and go selectors'
IMAGE_ENGINE=/usr/bin/podman GO_CMD=/usr/bin/go expect_make \
	'make test-e2e still routes every step to the stubs' test-e2e
unset IMAGE_ENGINE GO_CMD
expect_log 'the stub engine handled the build' '^PODMAN build '
expect_log 'the stub go handled the test invocation' '^GO test '

begin_case 'the suite is hermetic against outer make command-line assignments'
MAKEFLAGS='GO_CMD=/usr/bin/go IMAGE_ENGINE=/usr/bin/podman'
export MAKEFLAGS
expect_make 'make test-e2e still routes every step to the stubs' test-e2e
unset MAKEFLAGS
expect_log 'the stub engine handled the build' '^PODMAN build '
expect_log 'the stub go handled the test invocation' '^GO test '

begin_case 'a failed rebuild over a native-childless list still leaves the ref absent'
seed_manifest localhost/t17:1 "linux/arm64 $D2"
seed_manifest localhost/t17b:1
STUB_BUILD_RC=1
export STUB_BUILD_RC
expect_make_fail 'the rebuild over the arm64-only list fails' container-build IMG=localhost/t17:1
expect_make_fail 'the rebuild over the empty list fails' container-build IMG=localhost/t17b:1
unset STUB_BUILD_RC
expect_log 'the arm64-only list is invalidated even though image exists cannot see it' \
	"^PODMAN manifest rm localhost/t17:1$"
expect_log 'the empty list is invalidated even though image exists cannot see it' \
	"^PODMAN manifest rm localhost/t17b:1$"
check 'the failed rebuild leaves the arm64-only ref absent' \
	'[[ ! -e $(ref_file localhost/t17:1) ]]'
check 'the failed rebuild leaves the empty ref absent' \
	'[[ ! -e $(ref_file localhost/t17b:1) ]]'

begin_case 'alias spellings of one reference share a single lock file'
expect_make 'the qualified spelling builds' container-build IMG=localhost/t18:1
expect_make 'the unqualified alias builds' container-build IMG=t18:1
expect_make 'the tagless qualified spelling builds' container-build IMG=localhost/t19
expect_make 'the tagless unqualified alias builds' container-build IMG=t19
check 'aliases hashed to one lock per distinct reference (two references here)' \
	'[[ $(ls -1 "$CASE_DIR/run/hw-discovery-locks" | wc -l) == 2 ]]'

begin_case 'help and dry runs are side-effect free'
expect_make 'make help succeeds' help
expect_make 'make -n container-build succeeds' -n container-build
expect_out 'the dry run reveals the manifest build' 'image\.sh build-manifest'
expect_make 'make -n test-e2e succeeds' -n test-e2e
expect_out 'the dry run reveals the e2e workflow' 'e2e-workflow\.sh suite'
expect_no_out 'no reference is allocated at make parse time' 'ttl\.sh/hw-discovery-'
expect_make 'make -n container-push succeeds' -n container-push
expect_make 'make -n e2e-image succeeds' -n e2e-image
expect_make 'make -n demo succeeds' -n demo
expect_make_fail 'dry runs still reject non-podman selectors' -n container-build CONTAINER_TOOL=docker
expect_make_fail 'dry runs still reject outer image overrides' -n test-e2e E2E_CONTROLLER_IMAGE=x
check 'nothing was executed by any help or dry run' '[[ ! -s $CASE_DIR/events.log ]]'

# --------------------------------------------------------------------------
printf '\n%d passed, %d failed\n' "$pass" "$fail"
if [[ $fail != 0 ]]; then
	exit 1
fi
