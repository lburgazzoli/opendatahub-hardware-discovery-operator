#!/usr/bin/env bash
# Pinned Helm CLI: always executed through
# `go run` at the exact published version, never an unpinned local
# binary. This wrapper is the single source of the pin, shared by the
# make targets and the other hack/scripts.
set -euo pipefail
exec go run helm.sh/helm/v4/cmd/helm@v4.3.1 "$@"
