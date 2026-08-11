#!/usr/bin/env bash
#
# Destroys the kind cluster created by hack/dev-up.sh, and with it the
# development Harbor. Nothing is persisted, so there is nothing else to clean.

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-harbor-operator-dev}"

command -v kind >/dev/null 2>&1 || {
	printf 'error: kind not found in PATH (try: mise install)\n' >&2
	exit 1
}

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
	printf 'kind cluster %s does not exist\n' "$CLUSTER"
	exit 0
fi

kind delete cluster --name "$CLUSTER"
