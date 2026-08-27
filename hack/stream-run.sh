#!/usr/bin/env bash
# stream-run.sh — waits for a NEW AgentRun then streams its SSE output to stdout.
# Use this to verify whether the operator is streaming tokens in real-time,
# bypassing Caddy and React entirely.
#
# Usage:
#   ./hack/stream-run.sh                          # namespace=default
#   ./hack/stream-run.sh my-namespace
#   ./hack/stream-run.sh default agent-orca-system

NAMESPACE=${1:-default}
OPERATOR_NS=${2:-agent-orca-system}
LOCAL_PORT=18083

# ── Port-forward ──────────────────────────────────────────────────────────────

echo "Starting port-forward to operator on localhost:$LOCAL_PORT ..."
kubectl port-forward -n "$OPERATOR_NS" svc/agent-orca-internal-api "$LOCAL_PORT:8083" \
    >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null; wait "$PF_PID" 2>/dev/null' EXIT INT TERM

for i in 1 2 3 4 5 6 7 8 9 10; do
    curl -sf "http://localhost:$LOCAL_PORT/api/runs?namespace=$NAMESPACE" -o /dev/null 2>/dev/null \
        && break
    sleep 0.5
done

# ── Snapshot existing run names so we ignore them ────────────────────────────

EXISTING=$(kubectl get agentrun -n "$NAMESPACE" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)

echo "Ready. Waiting for a NEW AgentRun in namespace '$NAMESPACE'..."
echo "Send a chat message now."
echo

# ── Watch for ADDED events, skip pre-existing runs ───────────────────────────

kubectl get agentrun -n "$NAMESPACE" \
    --watch \
    --output-watch-events \
    -o jsonpath='{.type} {.object.metadata.name}{"\n"}' 2>/dev/null \
| while read -r event_type run_name; do
    [ "$event_type" != "ADDED" ] && continue
    [ -z "$run_name" ] && continue

    # Skip runs that existed before we started.
    if echo "$EXISTING" | grep -qxF "$run_name"; then
        continue
    fi

    echo "=== new run: $run_name — connecting to stream ==="

    curl -sN --no-buffer \
        "http://localhost:$LOCAL_PORT/api/runs/$run_name/stream?namespace=$NAMESPACE" \
    | while IFS= read -r line; do
        [ "${line#data:}" = "$line" ] && continue
        data="${line#data: }"

        type=$(printf '%s' "$data" \
            | python3 -c "import sys,json; print(json.load(sys.stdin).get('type',''))" 2>/dev/null)
        content=$(printf '%s' "$data" \
            | python3 -c "
import sys, json
d = json.load(sys.stdin)
print(d.get('content') or d.get('output') or d.get('message') or '', end='')
" 2>/dev/null)

        case "$type" in
            token)          printf '%s' "$content" ;;
            model_selected) printf '\n[model: %s]\n' "$content" ;;
            final_output)   printf '\n\n=== done ===\n\n'; break ;;
            error)          printf '\nERROR: %s\n' "$content"; break ;;
        esac
    done

    echo
    echo "Send another message or Ctrl+C to quit."
done
