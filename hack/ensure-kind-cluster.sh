#!/usr/bin/env bash
# Ensures the local kind cluster used by Skaffold exists (same defaults as hack/dev-kind.sh).
# Set SKIP_ENSURE_KIND=1 to skip. Override cluster name with KIND_CLUSTER (default: agent-orc-dev).

set -euo pipefail

if [[ -n "${SKIP_ENSURE_KIND:-}" ]]; then
  exit 0
fi

CLUSTER_NAME="${KIND_CLUSTER:-agent-orc-dev}"

if ! command -v kind &>/dev/null; then
  echo "ensure-kind-cluster: 'kind' not found in PATH (install kind or set SKIP_ENSURE_KIND=1)" >&2
  exit 1
fi

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  exit 0
fi

HOST_ARCH="$(uname -m)"
export DOCKER_DEFAULT_PLATFORM="linux/${HOST_ARCH}"
echo "ensure-kind-cluster: creating kind cluster '${CLUSTER_NAME}'..."
kind create cluster --name "${CLUSTER_NAME}" --wait 60s
