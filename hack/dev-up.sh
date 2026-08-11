#!/usr/bin/env bash
#
# Brings up a kind cluster with a development Harbor.
#
# The Harbor is deliberately throwaway: no persistence, no TLS, and the admin
# account is used for authentication. Robot account permissions (the operator
# needs a system-level robot) cannot be exercised here.
#
# Tear it down again with hack/dev-down.sh.

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-harbor-operator-dev}"
NAMESPACE="${HARBOR_NAMESPACE:-harbor}"
CHART_VERSION="${HARBOR_CHART_VERSION:-1.19.2}"
NODE_PORT="${HARBOR_NODE_PORT:-30002}"
ADMIN_PASSWORD="${HARBOR_PASSWORD:-Harbor12345}"

# The node port is published on the host by the kind cluster, so the same URL
# works from the host and as Harbor's externalURL.
HARBOR_URL="http://127.0.0.1:${NODE_PORT}"

log() { printf '\n==> %s\n' "$*"; }

require() {
	command -v "$1" >/dev/null 2>&1 || {
		printf 'error: %s not found in PATH (try: mise install)\n' "$1" >&2
		exit 1
	}
}

require docker
require kind
require helm
require kubectl

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
	log "kind cluster ${CLUSTER} already exists"
else
	log "creating kind cluster ${CLUSTER}"
	kind create cluster --name "$CLUSTER" --config - <<-EOF
		kind: Cluster
		apiVersion: kind.x-k8s.io/v1alpha4
		nodes:
		  - role: control-plane
		    extraPortMappings:
		      - containerPort: ${NODE_PORT}
		        hostPort: ${NODE_PORT}
		        listenAddress: "127.0.0.1"
	EOF
fi

kubectl config use-context "kind-${CLUSTER}" >/dev/null

log "installing Harbor chart ${CHART_VERSION}"
helm repo add harbor https://helm.goharbor.io >/dev/null
helm repo update harbor >/dev/null

# --wait covers the workloads; the API needs a moment longer to answer.
helm upgrade --install harbor harbor/harbor \
	--version "$CHART_VERSION" \
	--namespace "$NAMESPACE" \
	--create-namespace \
	--wait \
	--timeout 15m \
	--set expose.type=nodePort \
	--set expose.tls.enabled=false \
	--set "expose.nodePort.ports.http.nodePort=${NODE_PORT}" \
	--set "externalURL=${HARBOR_URL}" \
	--set persistence.enabled=false \
	--set trivy.enabled=false \
	--set "harborAdminPassword=${ADMIN_PASSWORD}"

log "waiting for the Harbor API"
for _ in $(seq 1 60); do
	if curl -fsS "${HARBOR_URL}/api/v2.0/health" >/dev/null 2>&1; then
		break
	fi
	sleep 5
done
curl -fsS "${HARBOR_URL}/api/v2.0/health" >/dev/null || {
	printf 'error: Harbor did not become healthy at %s\n' "$HARBOR_URL" >&2
	exit 1
}

cat <<EOF

Harbor is up.

  URL:      ${HARBOR_URL}
  User:     admin
  Password: ${ADMIN_PASSWORD}
  Context:  kind-${CLUSTER}

Run the client against it with:

  HARBOR_URL=${HARBOR_URL} \\
  HARBOR_USERNAME=admin \\
  HARBOR_PASSWORD=${ADMIN_PASSWORD} \\
  go test -count=1 -v -run TestIntegration ./internal/harbor/

EOF
