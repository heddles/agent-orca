#!/usr/bin/env bash
# Start the software factory on one GitHub issue (config/samples/software-factory).
#
#   hack/factory-run.sh 123                 # render and apply
#   hack/factory-run.sh 123 --dry-run       # print the workflow only 
#   hack/factory-run.sh                     # list open issues with the label
#
# Work starts only when the issue carries the trigger label and the person who
# applied it can write to the repository. Connectors that do this server side are
# planned (I2); until then this script is the allowlist.
set -euo pipefail

REPO="${REPO:-heddles/agent-orca}"
LABEL="${LABEL:-agentorca-aifactory}"
export BASE_REF="${BASE_REF:-main}"
export SANDBOX_IMAGE="${SANDBOX_IMAGE:-ghcr.io/heddles/agent-orca/dev-sandbox:v0.8.0-18-gde569b47-dirty@sha256:68651e36bfd7e46d38bffe038feaedf7ddcf7818e4efbc79adb526e0891cb076}"
export TEST_CMD="${TEST_CMD:-go vet ./internal/openshell/... && go test ./internal/openshell/...}"
export BUDGET_USD="${BUDGET_USD:-10.00}"
KUBE_CONTEXT="${KUBE_CONTEXT:-pnw-dev-master1}"

if [[ $# -eq 0 ]]; then
  gh issue list --repo "$REPO" --label "$LABEL" --state open
  exit 0
fi
issue="$1"; shift
dry=0
for a in "$@"; do
  case "$a" in
    --dry-run) dry=1 ;;
    *) echo "unknown option $a" >&2; exit 2 ;;
  esac
done
[[ "$issue" =~ ^[0-9]+$ ]] || { echo "issue must be a number" >&2; exit 2; }

labels=$(gh issue view "$issue" --repo "$REPO" --json labels,state --jq '[.state, (.labels[].name)] | join(" ")')
[[ "$labels" == OPEN* && " $labels " == *" $LABEL "* ]] || { echo "issue #$issue is not open with label $LABEL" >&2; exit 1; }

actor=$(gh api "repos/$REPO/issues/$issue/events" --paginate \
  --jq "[.[] | select(.event==\"labeled\" and .label.name==\"$LABEL\")] | last | .actor.login")
perm=$(gh api "repos/$REPO/collaborators/$actor/permission" --jq .permission)
case "$perm" in admin|maintain|write) ;; *) echo "$actor applied the label but has $perm access" >&2; exit 1 ;; esac

export ISSUE="$issue" REPO TRIGGER_ACTOR="$actor"
export NAME="factory-issue-$issue-$(date +%m%d%H%M)"
export BRANCH="${BRANCH:-${BRANCH_PREFIX:-factory}/issue-$issue}"
export ISSUE_URL="https://github.com/$REPO/issues/$issue"
vars='${NAME} ${ISSUE} ${REPO} ${ISSUE_URL} ${TRIGGER_ACTOR} ${BRANCH} ${BASE_REF} ${SANDBOX_IMAGE} ${TEST_CMD} ${BUDGET_USD}'
tmpl=workflow.yaml.tmpl
manifest=$(envsubst "$vars" < "$(dirname "$0")/../config/samples/software-factory/$tmpl")

if [[ "$dry" == 1 ]]; then echo "$manifest"; exit 0; fi
echo "$manifest" | kubectl --context "$KUBE_CONTEXT" apply -f -
echo "watch: kubectl --context $KUBE_CONTEXT -n agent-orca-system get agentworkflow $NAME -w"
