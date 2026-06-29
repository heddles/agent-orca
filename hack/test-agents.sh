#!/usr/bin/env bash
# hack/test-agents.sh — quickly generate and test agents, runs, and deployments
#
# Usage:
#   ./hack/test-agents.sh run [--watch]              # Create and test an AgentRun
#   ./hack/test-agents.sh deploy [--watch]           # Create and test an AgentDeployment
#   ./hack/test-agents.sh agent [--watch]            # Create just an Agent (no run)
#   ./hack/test-agents.sh cleanup                    # Delete test resources
#   ./hack/test-agents.sh list                       # List all test resources
#
# Options:
#   --watch     Watch resource status until completion (or Ctrl-C)
#   -n, --namespace   Target namespace (default: default)
#   -h, --help  Show this help
#
# Examples:
#   ./hack/test-agents.sh run --watch                # Create run and watch
#   ./hack/test-agents.sh deploy -n myns --watch     # Deploy in custom namespace
#   ./hack/test-agents.sh cleanup -n myns            # Clean up in custom namespace

set -euo pipefail

# ── config ────────────────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-default}"
KUBECONFIG_CONTEXT=""  # empty = use current context
TIMESTAMP=$(date +%s%N | tail -c 7)  # last 7 digits of nanoseconds for uniqueness
COLOR_RESET="\033[0m"
COLOR_CYAN="\033[36m"
COLOR_GREEN="\033[32m"
COLOR_YELLOW="\033[33m"

# ── helpers ───────────────────────────────────────────────────────────────────
log_step()  { echo -e "${COLOR_CYAN}▶ $*${COLOR_RESET}" >&2; }
log_ok()    { echo -e "${COLOR_GREEN}✓ $*${COLOR_RESET}" >&2; }
log_warn()  { echo -e "${COLOR_YELLOW}⚠ $*${COLOR_RESET}" >&2; }
log_info()  { echo "  $*" >&2; }

kubectl_cmd() {
  if [[ -n "$KUBECONFIG_CONTEXT" ]]; then
    kubectl --context "$KUBECONFIG_CONTEXT" "$@"
  else
    kubectl "$@"
  fi
}

# ── parse args ────────────────────────────────────────────────────────────────
COMMAND="${1:-}"
WATCH=false
shift || true

case "$COMMAND" in
  run|deploy|agent|cleanup|list) ;;
  -h|--help)
    sed -n '2,19p' "$0"
    exit 0
    ;;
  *)
    echo "Unknown command: $COMMAND"
    echo "Run with -h for help"
    exit 1
    ;;
esac

while [[ $# -gt 0 ]]; do
  case "$1" in
    --watch)
      WATCH=true
      shift
      ;;
    -n|--namespace)
      NAMESPACE="$2"
      shift 2
      ;;
    -h|--help)
      sed -n '2,19p' "$0"
      exit 0
      ;;
    *)
      echo "Unknown option: $1"
      exit 1
      ;;
  esac
done

# ── ensure namespace exists ────────────────────────────────────────────────────
if ! kubectl_cmd get namespace "$NAMESPACE" &>/dev/null; then
  log_step "Creating namespace '$NAMESPACE'"
  kubectl_cmd create namespace "$NAMESPACE"
  log_ok "Namespace created"
fi

# ── create agent resource ─────────────────────────────────────────────────────
create_agent() {
  local agent_name="test-agent-${TIMESTAMP}"

  log_step "Creating Agent '$agent_name'"

  # Use a temp file to avoid YAML escaping issues with multiline Python
  cat > /tmp/agent-yaml-$TIMESTAMP.yaml <<'YAML'
apiVersion: agentorc.agentorc.io/v1alpha1
kind: Agent
metadata:
  name: AGENT_NAME_PLACEHOLDER
  namespace: NAMESPACE_PLACEHOLDER
spec:
  modelSelectorRef: default
  systemPrompt: "You are a helpful assistant. Answer concisely."
  runtime:
    ociRef: "python:3.12-slim"
    framework: openai-compatible
    command: ["python3", "-c"]
    args:
      - |
        import json, os, urllib.request, sys
        inp = os.environ.get("AGENTORC_INPUT", "hello")
        api_key = os.environ.get("OPENAI_API_KEY", "unused")
        try:
          body = json.dumps({"model": "default", "messages": [{"role": "user", "content": inp}]}).encode()
          req = urllib.request.Request("http://localhost:8080/v1/chat/completions", data=body, headers={"Content-Type": "application/json", "Authorization": f"Bearer {api_key}"})
          with urllib.request.urlopen(req, timeout=5) as r:
            resp = json.loads(r.read())
          print(resp["choices"][0]["message"]["content"])
        except urllib.error.HTTPError as e:
          print(f"HTTP Error {e.code}: {e.reason}", file=sys.stderr)
          sys.exit(1)
        except Exception as e:
          print(f"Error: {e}", file=sys.stderr)
          sys.exit(1)
  resources:
    requests:
      cpu: 50m
      memory: 64Mi
    limits:
      cpu: 200m
      memory: 128Mi
YAML

  # Replace placeholders
  sed -i.bak "s/AGENT_NAME_PLACEHOLDER/$agent_name/g; s/NAMESPACE_PLACEHOLDER/$NAMESPACE/g" /tmp/agent-yaml-$TIMESTAMP.yaml

  kubectl_cmd apply -f /tmp/agent-yaml-$TIMESTAMP.yaml >/dev/null 2>&1
  rm -f /tmp/agent-yaml-$TIMESTAMP.yaml /tmp/agent-yaml-$TIMESTAMP.yaml.bak

  log_ok "Agent created: $agent_name"
  echo "$agent_name"
}

# ── test agentrun ─────────────────────────────────────────────────────────────
test_agentrun() {
  local agent_name
  agent_name=$(create_agent)

  local run_name="test-run-${TIMESTAMP}"
  log_step "Creating AgentRun '$run_name'"

  printf '%s\n' \
    'apiVersion: agentorc.agentorc.io/v1alpha1' \
    'kind: AgentRun' \
    'metadata:' \
    "  name: $run_name" \
    "  namespace: $NAMESPACE" \
    'spec:' \
    "  agentRef: $agent_name" \
    '  input: "What is 2 + 2?"' | kubectl_cmd apply -f -

  log_ok "AgentRun created: $run_name"

  if [[ "$WATCH" == "true" ]]; then
    log_info "Watching AgentRun status (press Ctrl-C to stop)..."
    kubectl_cmd get agentrun "$run_name" -n "$NAMESPACE" -w
  else
    log_info "Check status with: kubectl get agentrun $run_name -n $NAMESPACE"
    log_info "Watch with: kubectl get agentrun $run_name -n $NAMESPACE -w"
  fi
}

# ── test agentdeployment ──────────────────────────────────────────────────────
test_agentdeployment() {
  local agent_name
  agent_name=$(create_agent)

  local deploy_name="test-deploy-${TIMESTAMP}"
  log_step "Creating AgentDeployment '$deploy_name'"

  printf '%s\n' \
    'apiVersion: agentorc.agentorc.io/v1alpha1' \
    'kind: AgentDeployment' \
    'metadata:' \
    "  name: $deploy_name" \
    "  namespace: $NAMESPACE" \
    'spec:' \
    "  agentRef: $agent_name" \
    '  inputSource:' \
    '    type: chat' \
    '  replicas: 1' \
    '  restartPolicy:' \
    '    minBackoffSeconds: 5' \
    '    maxBackoffSeconds: 300' \
    '    maxConsecutiveFailures: 5' \
    '  checkpointTTL: 604800' | kubectl_cmd apply -f -

  log_ok "AgentDeployment created: $deploy_name"

  if [[ "$WATCH" == "true" ]]; then
    log_info "Watching AgentDeployment status (press Ctrl-C to stop)..."
    kubectl_cmd get agentdeployment "$deploy_name" -n "$NAMESPACE" -w
  else
    log_info "Check status with: kubectl get agentdeployment $deploy_name -n $NAMESPACE"
    log_info "Watch with: kubectl get agentdeployment $deploy_name -n $NAMESPACE -w"
    log_info "Watch pods with: kubectl get pods -n $NAMESPACE -l agentdeployment.agentorc.io=$deploy_name -w"
  fi
}

# ── just create agent ─────────────────────────────────────────────────────────
create_agent_only() {
  local agent_name
  agent_name=$(create_agent)
  log_info "Created agent: $agent_name"
  log_info "Create a run: kubectl apply -f - <<'EOF'"
  log_info "apiVersion: agentorc.agentorc.io/v1alpha1"
  log_info "kind: AgentRun"
  log_info "metadata:"
  log_info "  name: my-run"
  log_info "  namespace: $NAMESPACE"
  log_info "spec:"
  log_info "  agentRef: $agent_name"
  log_info "  input: \"Your question here\""
  log_info "EOF"
}

# ── cleanup ───────────────────────────────────────────────────────────────────
cleanup_test_resources() {
  log_step "Cleaning up test resources in namespace '$NAMESPACE'"

  # Delete AgentRuns and AgentDeployments (leave Agents for now)
  kubectl_cmd delete agentrun,agentdeployment \
    -n "$NAMESPACE" \
    -l test-generated=true \
    --ignore-not-found 2>/dev/null || true

  log_ok "Test resources cleaned up"
}

# ── list resources ────────────────────────────────────────────────────────────
list_resources() {
  log_step "Test resources in namespace '$NAMESPACE'"
  echo

  echo "Agents:"
  kubectl_cmd get agents -n "$NAMESPACE" --no-headers 2>/dev/null || log_warn "No agents found"

  echo
  echo "AgentRuns:"
  kubectl_cmd get agentruns -n "$NAMESPACE" --no-headers 2>/dev/null || log_warn "No agent runs found"

  echo
  echo "AgentDeployments:"
  kubectl_cmd get agentdeployments -n "$NAMESPACE" --no-headers 2>/dev/null || log_warn "No agent deployments found"
}

# ── main ──────────────────────────────────────────────────────────────────────
case "$COMMAND" in
  run)
    test_agentrun
    ;;
  deploy)
    test_agentdeployment
    ;;
  agent)
    create_agent_only
    ;;
  cleanup)
    cleanup_test_resources
    ;;
  list)
    list_resources
    ;;
esac
