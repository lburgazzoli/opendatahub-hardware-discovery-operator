#!/usr/bin/env bash
# Package dist/chart into dist/ and verify the archive: no scratch, key
# or build artifacts; Chart.yaml and values.yaml present; usable
# standalone (lint + render from the archive itself).
set -euo pipefail
cd "$(dirname "$0")/../.."

name=$(sed -n 's/^name: *//p' dist/chart/Chart.yaml)
version=$(sed -n 's/^version: *//p' dist/chart/Chart.yaml)
archive=dist/$name-$version.tgz

./hack/scripts/helm.sh package dist/chart -d dist/

test -f "$archive" || { echo "ERROR: expected archive $archive not found" >&2; exit 1; }

# The archive must contain only the reviewed chart: no repository-local
# scratch, credentials or build outputs.
pattern='(\.context|\.git/|\.kube|kubeconfig|\.env|id_rsa|\.pem|install\.yaml|rendered\.yaml)'
if tar -tzf "$archive" | grep -Eq "$pattern"; then
	echo "ERROR: archive $archive contains scratch, key or build artifacts:" >&2
	tar -tzf "$archive" | grep -E "$pattern" >&2
	exit 1
fi
tar -tzf "$archive" | grep -Eq "^[^/]*/Chart.yaml$" || { echo "ERROR: archive has no Chart.yaml" >&2; exit 1; }
tar -tzf "$archive" | grep -Eq "^[^/]*/values.yaml$" || { echo "ERROR: archive has no values.yaml" >&2; exit 1; }

echo "Archive inventory:"
tar -tzf "$archive" | sort

./hack/scripts/helm.sh lint "$archive"
./hack/scripts/helm.sh template hardware-discovery "$archive" \
	--namespace opendatahub-hardware-discovery-operator-system > dist/rendered-from-archive.yaml
echo "Archive linted and rendered standalone (dist/rendered-from-archive.yaml)"
