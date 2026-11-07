#!/usr/bin/env bash
# Lint packaging/module.yaml against dist/chart with the pinned
# odh-platform-tools, offline.
#
# The pinned version is the commit backing pseudo-version
# v0.0.0-20261028215308-948ecd9b3ace; the module zip cannot build it
# (its CRD bases are gitignored upstream but go:embed'ed), so the pinned
# local checkout is the execution vehicle, run in place through `go
# run` — nothing is copied into this repository. The checkout must sit
# exactly at the pin with a clean tree, so the compiled tool is provably
# the pinned commit. Its go:embed'ed CRD base is generated in the
# checkout when missing (a gitignored build output of that repository,
# like its build cache).
set -euo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
cd "$repo"

commit=948ecd9b3acec53cdee868bca8d54e4aff74eabd
dir=${ODH_PLATFORM_TOOLS_DIR:-$HOME/work/dev/rhai/odh-platform-tools}

test -d "$dir" || {
	echo "ERROR: odh-platform-tools checkout not found at $dir" >&2
	echo "       (override with ODH_PLATFORM_TOOLS_DIR=<path>)" >&2
	exit 1
}

actual=$(git -C "$dir" rev-parse HEAD 2>/dev/null)
test "$actual" = "$commit" || {
	echo "ERROR: odh-platform-tools at $dir is $actual" >&2
	echo "       expected the pinned commit $commit" >&2
	exit 1
}

# A clean tree at the pinned HEAD is what makes `go run` from the
# checkout equivalent to running the pinned commit: local edits must not
# silently change lint results. Ignored files (the generated CRD base
# below) do not dirty the tree.
test -z "$(git -C "$dir" status --porcelain)" || {
	echo "ERROR: odh-platform-tools at $dir has local changes" >&2
	echo "       module-lint must run the exact pinned commit $commit" >&2
	echo "       (commit or stash them, or point ODH_PLATFORM_TOOLS_DIR at a clean checkout)" >&2
	exit 1
}

base=$dir/config/crd/bases/config.opendatahub.io_platformmoduledefinitions.yaml
test -f "$base" || make -C "$dir" manifests

cd "$dir" && go run ./cmd module lint --config "$repo/packaging/module.yaml"
