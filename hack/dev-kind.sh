#!/usr/bin/env bash
# hack/dev-kind.sh — test, build, and deploy agent-orc into a local kind cluster.
#
# Usage:
#   ./hack/dev-kind.sh [cluster|local] [--skip-tests] [--skip-build] [--no-cert-manager] [--reset]
#
# Modes:
#   cluster  (default) — full in-cluster deploy via Helm
#   local              — install CRDs only, run operator on the host (fast iteration)
#
# Flags:
#   --skip-tests       skip "make test"
#   --skip-build       skip docker build + kind load (cluster mode only)
#   --no-cert-manager  skip cert-manager installation; Helm generates a self-signed cert
#   --reset            delete and recreate the kind cluster first
#
# Examples:
#   ./hack/dev-kind.sh                              # full deploy with cert-manager
#   ./hack/dev-kind.sh --no-cert-manager            # full deploy, self-signed cert via Helm
#   ./hack/dev-kind.sh local                        # run operator on host
#   ./hack/dev-kind.sh cluster --skip-build         # re-deploy without rebuilding images
#   ./hack/dev-kind.sh --reset                      # nuke cluster and start fresh

set -euo pipefail

# ── configuration ────────────────────────────────────────────────────────────
CLUSTER_NAME="${KIND_CLUSTER:-agent-orc-dev}"
NAMESPACE="agent-orc-system"
OPERATOR_IMG="agent-orc/operator:dev"
MODEL_ROUTER_IMG="agent-orc/model-router:dev"
UI_IMG="agent-orc/ui:dev"
HELM_RELEASE="agent-orc"
CERT_MANAGER_VERSION="v1.20.0"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOST_ARCH="$(uname -m)"

# ── parse args ────────────────────────────────────────────────────────────────
MODE="cluster"
SKIP_TESTS=false
SKIP_BUILD=false
RESET=false
USE_CERT_MANAGER=true

for arg in "$@"; do
  case "$arg" in
    cluster)           MODE="cluster" ;;
    local)             MODE="local" ;;
    --skip-tests)      SKIP_TESTS=true ;;
    --skip-build)      SKIP_BUILD=true ;;
    --no-cert-manager) USE_CERT_MANAGER=false ;;
    --reset)           RESET=true ;;
    -h|--help)
      sed -n '2,25p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *) echo "Unknown argument: $arg"; exit 1 ;;
  esac
done

# ── colours ───────────────────────────────────────────────────────────────────
bold=$(tput bold 2>/dev/null || true)
green=$(tput setaf 2 2>/dev/null || true)
yellow=$(tput setaf 3 2>/dev/null || true)
cyan=$(tput setaf 6 2>/dev/null || true)
reset=$(tput sgr0 2>/dev/null || true)

step()  { echo "${bold}${cyan}▶ $*${reset}"; }
ok()    { echo "${green}  ✓ $*${reset}"; }
warn()  { echo "${yellow}  ⚠ $*${reset}"; }

# ── prerequisite check ────────────────────────────────────────────────────────
step "Checking prerequisites"
missing=()
for cmd in go docker kind kubectl; do
  command -v "$cmd" &>/dev/null || missing+=("$cmd")
done
if [[ "$MODE" == "cluster" ]]; then
  command -v helm &>/dev/null || missing+=("helm")
fi
if [[ "$USE_CERT_MANAGER" == "true" ]] && ! command -v helm &>/dev/null; then
  USE_CERT_MANAGER=false
  warn "helm not found — falling back to --no-cert-manager (self-signed cert)"
fi
if [[ ${#missing[@]} -gt 0 ]]; then
  echo "Missing required tools: ${missing[*]}"
  echo "Install them and re-run."
  exit 1
fi
ok "All prerequisites found"

cd "$REPO_ROOT"

# ── tests ─────────────────────────────────────────────────────────────────────
if [[ "$SKIP_TESTS" == "false" ]]; then
  step "Running unit tests (make test)"
  make test
  ok "Tests passed"

  step "Running UI tests (make test-ui)"
  make test-ui
  ok "UI tests passed"
else
  warn "Skipping tests (--skip-tests)"
fi

# ── kind cluster ──────────────────────────────────────────────────────────────
if [[ "$RESET" == "true" ]]; then
  step "Resetting kind cluster '${CLUSTER_NAME}'"
  kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
fi

step "Ensuring kind cluster '${CLUSTER_NAME}'"
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  ok "Cluster already exists"
else
  DOCKER_DEFAULT_PLATFORM="linux/${HOST_ARCH}" kind create cluster --name "${CLUSTER_NAME}" --wait 60s
  ok "Cluster created"
fi

KUBECONFIG_FLAG="--context kind-${CLUSTER_NAME}"
kubectl ${KUBECONFIG_FLAG} cluster-info --request-timeout=5s >/dev/null
ok "kubectl connected to kind-${CLUSTER_NAME}"

# ── CRDs ─────────────────────────────────────────────────────────────────────
step "Installing CRDs"
make manifests generate
make install
ok "CRDs installed"

# ─────────────────────────────────────────────────────────────────────────────
# MODE: local — run operator on host
# ─────────────────────────────────────────────────────────────────────────────
if [[ "$MODE" == "local" ]]; then
  step "Applying quickstart samples"
  kubectl ${KUBECONFIG_FLAG} apply -f hack/samples/quickstart.yaml
  ok "Samples applied"

  echo
  echo "${bold}Operator running locally. Press Ctrl-C to stop.${reset}"
  echo "  Metrics:      http://localhost:8080/metrics"
  echo "  Health:       http://localhost:8081/healthz"
  echo "  UI API:       http://localhost:8083/api/runs"
  echo
  echo "  Watch:  kubectl ${KUBECONFIG_FLAG} get agentrun,agent,modelprovider,modelselector -A"
  echo

  # Webhook validation is out-of-cluster so it won't be reached from the API server.
  # Patch the ValidatingWebhookConfiguration to Ignore so CRD creates succeed.
  if kubectl ${KUBECONFIG_FLAG} get validatingwebhookconfiguration 2>/dev/null | grep -q agent-orc; then
    warn "Patching ValidatingWebhookConfiguration to failurePolicy=Ignore for local mode"
    for wh in $(kubectl ${KUBECONFIG_FLAG} get validatingwebhookconfiguration -o name | grep agent-orc); do
      kubectl ${KUBECONFIG_FLAG} patch "$wh" \
        --type='json' \
        -p='[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Ignore"},{"op":"replace","path":"/webhooks/1/failurePolicy","value":"Ignore"},{"op":"replace","path":"/webhooks/2/failurePolicy","value":"Ignore"}]' \
        2>/dev/null || true
    done
  fi

  # Pass through Redis env vars for token streaming if set.
  # To enable: export STATE_BACKEND=redis REDIS_URL=redis://localhost:6379
  MODEL_ROUTER_IMAGE="${MODEL_ROUTER_IMG}" \
    STATE_BACKEND="${STATE_BACKEND:-}" \
    REDIS_URL="${REDIS_URL:-}" \
    go run ./cmd/main.go \
      --leader-elect=false \
      --health-probe-bind-address=:8081 \
      --metrics-secure=false \
      --metrics-bind-address=:8080

  exit 0
fi

# ─────────────────────────────────────────────────────────────────────────────
# MODE: cluster — full in-cluster deploy via Helm
# ─────────────────────────────────────────────────────────────────────────────

# ── detect host platform ─────────────────────────────────────────────────────
case "${HOST_ARCH}" in
  x86_64)  BUILD_PLATFORM="linux/amd64" ;;
  aarch64|arm64) BUILD_PLATFORM="linux/arm64" ;;
  *) echo "Unsupported architecture: ${HOST_ARCH}"; exit 1 ;;
esac

# ── buildx setup ──────────────────────────────────────────────────────────────
if [[ "$SKIP_BUILD" == "false" ]]; then
  step "Ensuring buildx builder (${BUILD_PLATFORM})"
  if docker buildx inspect agent-orc-builder &>/dev/null; then
    docker buildx use agent-orc-builder
  else
    docker buildx create --name agent-orc-builder \
      --driver docker-container \
      --platform "${BUILD_PLATFORM}" \
      --use
  fi
  docker buildx inspect --bootstrap >/dev/null
  ok "Buildx builder ready (${BUILD_PLATFORM})"

  # Build each image as an OCI tar then load into kind.
  OPERATOR_TAR="$(mktemp /tmp/agent-orc-operator-XXXXXX)"
  ROUTER_TAR="$(mktemp /tmp/agent-orc-router-XXXXXX)"
  UI_TAR="$(mktemp /tmp/agent-orc-ui-XXXXXX)"
  # DEMO_AGENT_TAR="$(mktemp /tmp/agent-orc-demo-agent-XXXXXX)"
  # Ensure temp files are removed even on error.
  trap 'rm -f "${OPERATOR_TAR}" "${ROUTER_TAR}" "${UI_TAR}"' EXIT

  step "Building operator image (${BUILD_PLATFORM})"
  docker buildx build \
    --platform "${BUILD_PLATFORM}" \
    --tag "${OPERATOR_IMG}" \
    --output "type=oci,dest=${OPERATOR_TAR}" \
    .
  ok "Operator image built → ${OPERATOR_TAR}"

  step "Building model-router image (${BUILD_PLATFORM})"
  docker buildx build \
    --platform "${BUILD_PLATFORM}" \
    --tag "${MODEL_ROUTER_IMG}" \
    --output "type=oci,dest=${ROUTER_TAR}" \
    -f cmd/model-router/Dockerfile \
    .
  ok "Model-router image built → ${ROUTER_TAR}"

  step "Building UI image (${BUILD_PLATFORM})"
  docker buildx build \
    --platform "${BUILD_PLATFORM}" \
    --tag "${UI_IMG}" \
    --output "type=oci,dest=${UI_TAR}" \
    -f Dockerfile.ui-proxy \
    .
  ok "UI image built → ${UI_TAR}"

  # step "Building demo-agent image (${BUILD_PLATFORM})"
  # docker buildx build \
  #   --platform "${BUILD_PLATFORM}" \
  #   --tag "agent-orc/demo-agent:dev" \
  #   --output "type=oci,dest=${DEMO_AGENT_TAR}" \
  #   -f Dockerfile.demo-agent \
  #   .
  # ok "Demo-agent image built → ${DEMO_AGENT_TAR}"

  step "Loading images into kind cluster '${CLUSTER_NAME}'"
  kind load image-archive "${OPERATOR_TAR}" --name "${CLUSTER_NAME}"
  kind load image-archive "${ROUTER_TAR}" --name "${CLUSTER_NAME}"
  kind load image-archive "${UI_TAR}" --name "${CLUSTER_NAME}"
  # kind load image-archive "${DEMO_AGENT_TAR}" --name "${CLUSTER_NAME}"
  ok "All four images loaded (${BUILD_PLATFORM})"
else
  warn "Skipping image build (--skip-build)"
fi

# ── cert-manager (optional) ───────────────────────────────────────────────────
if [[ "$USE_CERT_MANAGER" == "true" ]]; then
  step "Installing cert-manager ${CERT_MANAGER_VERSION}"
  if kubectl ${KUBECONFIG_FLAG} get namespace cert-manager &>/dev/null \
      && kubectl ${KUBECONFIG_FLAG} get deployment -n cert-manager cert-manager &>/dev/null; then
    ok "cert-manager already installed"
  else
    helm repo add jetstack https://charts.jetstack.io --force-update
    helm repo update jetstack
    helm upgrade --install cert-manager jetstack/cert-manager \
      --namespace cert-manager --create-namespace \
      --version "${CERT_MANAGER_VERSION}" \
      --set crds.enabled=true \
      --kube-context "kind-${CLUSTER_NAME}" \
      --wait
    ok "cert-manager installed"
  fi
else
  warn "Skipping cert-manager — Helm will generate a self-signed cert (webhook.certManager=false)"
fi

# ── helm deploy ───────────────────────────────────────────────────────────────
step "Deploying agent-orc via Helm (namespace: ${NAMESPACE})"
helm upgrade --install "${HELM_RELEASE}" ./charts/agent-orc \
  --namespace "${NAMESPACE}" --create-namespace \
  --kube-context "kind-${CLUSTER_NAME}" \
  --set operator.image.repository="$(echo ${OPERATOR_IMG} | cut -d: -f1)" \
  --set operator.image.tag="$(echo ${OPERATOR_IMG} | cut -d: -f2)" \
  --set operator.image.pullPolicy=Never \
  --set modelRouter.image.repository="$(echo ${MODEL_ROUTER_IMG} | cut -d: -f1)" \
  --set modelRouter.image.tag="$(echo ${MODEL_ROUTER_IMG} | cut -d: -f2)" \
  --set modelRouter.image.pullPolicy=Never \
  --set ui.image.repository="$(echo ${UI_IMG} | cut -d: -f1)" \
  --set ui.image.tag="$(echo ${UI_IMG} | cut -d: -f2)" \
  --set ui.image.pullPolicy=Never \
  --set operator.leaderElection=false \
  --set webhook.certManager="${USE_CERT_MANAGER}" \
  --wait --timeout=120s
ok "agent-orc deployed"

# ── wait for operator ─────────────────────────────────────────────────────────
step "Waiting for operator and UI pods to be ready"
if [[ "$SKIP_BUILD" == "false" ]]; then
  # Force a rollout so the newly-loaded kind images (same tag) are actually used.
  kubectl ${KUBECONFIG_FLAG} rollout restart deployment \
    -n "${NAMESPACE}" "${HELM_RELEASE}-operator" "${HELM_RELEASE}-ui"
fi
kubectl ${KUBECONFIG_FLAG} rollout status deployment \
  -n "${NAMESPACE}" "${HELM_RELEASE}-operator" --timeout=90s
kubectl ${KUBECONFIG_FLAG} rollout status deployment \
  -n "${NAMESPACE}" "${HELM_RELEASE}-ui" --timeout=90s
ok "Operator and UI pods ready"

# ── samples (with webhook retry) ─────────────────────────────────────────────
# The pod readiness probe (port 8081) passes before kube-proxy has fully
# programmed iptables rules for the webhook port (9443→443).  Retry until the
# apply succeeds or we exhaust attempts.
step "Applying quickstart samples (retrying until webhook is reachable)"
# Create the credentials Secret only if it doesn't already exist, so a real API
# key set by the developer is never overwritten on subsequent deployments.
if ! kubectl ${KUBECONFIG_FLAG} get secret anthropic-dev-key -n default &>/dev/null; then
  kubectl ${KUBECONFIG_FLAG} apply -f - <<'SECRETEOF'
apiVersion: v1
kind: Secret
metadata:
  name: anthropic-dev-key
  namespace: default
type: Opaque
stringData:
  api-key: "sk-ant-placeholder-replace-me"
SECRETEOF
  warn "Created placeholder secret 'anthropic-dev-key'. Replace the api-key value to make real LLM calls."
else
  ok "Secret 'anthropic-dev-key' already exists — preserving existing api-key"
fi

# Create the OpenAI credentials Secret only if it doesn't already exist.
if ! kubectl ${KUBECONFIG_FLAG} get secret openai-dev-key -n default &>/dev/null; then
  kubectl ${KUBECONFIG_FLAG} apply -f - <<'SECRETEOF'
apiVersion: v1
kind: Secret
metadata:
  name: openai-dev-key
  namespace: default
type: Opaque
stringData:
  api-key: "sk-ant-placeholder-replace-me"
SECRETEOF
  warn "Created placeholder secret 'openai-dev-key'. Replace the api-key value to make real LLM calls."
else
  ok "Secret 'openai-dev-key' already exists — preserving existing api-key"
fi

max_attempts=15
attempt=0
until kubectl ${KUBECONFIG_FLAG} apply -f hack/samples/quickstart.yaml; do
  attempt=$((attempt + 1))
  if [[ ${attempt} -ge ${max_attempts} ]]; then
    echo "Failed to apply samples after ${max_attempts} attempts — check operator logs"
    exit 1
  fi
  warn "Webhook not reachable yet, retrying in 5s (attempt ${attempt}/${max_attempts})..."
  sleep 5
done
ok "Samples applied"

# ── port-forward UI ──────────────────────────────────────────────────────────
step "Port-forwarding UI to http://localhost:8080"
kubectl ${KUBECONFIG_FLAG} port-forward -n "${NAMESPACE}" "svc/${HELM_RELEASE}-ui" 8080:80 &
UI_PF_PID=$!
trap 'kill "${UI_PF_PID}" 2>/dev/null || true' EXIT
sleep 2
ok "UI available at http://localhost:8080 (PID ${UI_PF_PID})"

# ── UI E2E tests ─────────────────────────────────────────────────────────────
if [[ "$SKIP_TESTS" == "false" ]]; then
  step "Running UI E2E tests (Playwright)"
  (cd ui && BASE_URL=http://localhost:8080 npm run test:e2e) && ok "UI E2E tests passed" || warn "UI E2E tests failed"
else
  warn "Skipping UI E2E tests (--skip-tests)"
fi

# ── status ────────────────────────────────────────────────────────────────────
echo
step "Cluster status"
echo
kubectl ${KUBECONFIG_FLAG} get pods -n "${NAMESPACE}"
echo
kubectl ${KUBECONFIG_FLAG} get agentrun,agent,modelprovider,modelselector -A
echo
echo "${bold}Useful commands:${reset}"
echo "  Operator logs:  kubectl --context kind-${CLUSTER_NAME} logs -n ${NAMESPACE} -l app.kubernetes.io/component=operator -f"
echo "  UI logs:        kubectl --context kind-${CLUSTER_NAME} logs -n ${NAMESPACE} -l app.kubernetes.io/component=ui -f"
echo "  UI:             http://localhost:8080 (port-forward running)"
echo "  AgentRun watch: kubectl --context kind-${CLUSTER_NAME} get agentrun -n default -w"
echo "  UI API:         kubectl --context kind-${CLUSTER_NAME} port-forward -n ${NAMESPACE} svc/${HELM_RELEASE}-operator 8083:8083"
echo "  Tear down:      kind delete cluster --name ${CLUSTER_NAME}"
echo
echo "${bold}Press Ctrl-C to stop port-forwarding and exit.${reset}"
ok "Done"
wait "${UI_PF_PID}"
