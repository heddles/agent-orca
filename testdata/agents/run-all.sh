#!/usr/bin/env bash
# run-all.sh — apply all framework test agents and optionally watch their AgentRuns.
#
# Usage:
#   ./testdata/agents/run-all.sh              # apply shared + all agents + all runs
#   ./testdata/agents/run-all.sh --watch      # apply then watch run status
#   ./testdata/agents/run-all.sh --cleanup    # delete all test resources
#   ./testdata/agents/run-all.sh <framework>  # apply only one framework (e.g. autogen)
#
# Frameworks: openai-compatible autogen semantic-kernel langgraph google-adk tools

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NS="${NAMESPACE:-default}"

FRAMEWORKS=(openai-compatible autogen semantic-kernel langgraph google-adk tools)

RUN_NAMES=(
  test-openai-compat-run
  test-autogen-run
  test-semantic-kernel-run
  test-langgraph-run
  test-google-adk-run
  test-mcp-run
  test-agent-tool-run
)

cleanup() {
  echo "==> Deleting test AgentRuns..."
  kubectl delete agentrun "${RUN_NAMES[@]}" -n "$NS" --ignore-not-found

  echo "==> Deleting test Agents..."
  kubectl delete agent \
    test-openai-compat test-autogen test-semantic-kernel test-langgraph test-google-adk \
    test-tools-orchestrator test-summarizer-agent \
    -n "$NS" --ignore-not-found

  echo "==> Deleting test Tools..."
  kubectl delete tool \
    test-mcp-calculator test-summarizer-tool \
    -n "$NS" --ignore-not-found

  echo "==> Deleting shared resources..."
  kubectl delete modelselector test-router -n "$NS" --ignore-not-found
  kubectl delete modelprovider test-claude-sonnet test-openai-gpt4 -n "$NS" --ignore-not-found

  echo "Done."
}

watch_runs() {
  echo ""
  echo "==> Watching AgentRun status (Ctrl-C to stop)..."
  kubectl get agentrun "${RUN_NAMES[@]}" -n "$NS" -w
}

apply_all() {
  echo "==> Applying shared ModelProvider + ModelSelector..."
  kubectl apply -f "$SCRIPT_DIR/shared.yaml"

  local target="${1:-all}"
  for fw in "${FRAMEWORKS[@]}"; do
    if [[ "$target" == "all" || "$target" == "$fw" ]]; then
      echo "==> Applying $fw agent + run..."
      kubectl apply -f "$SCRIPT_DIR/${fw}.yaml"
    fi
  done
}

case "${1:-}" in
  --cleanup)
    cleanup
    ;;
  --watch)
    apply_all "all"
    watch_runs
    ;;
  "")
    apply_all "all"
    ;;
  *)
    # single framework or unknown flag
    if [[ "${1}" == --* ]]; then
      echo "Unknown flag: $1"
      echo "Usage: $0 [--watch|--cleanup|<framework>]"
      exit 1
    fi
    apply_all "$1"
    ;;
esac
