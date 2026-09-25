#!/usr/bin/env bash
# scripts/smoke-channel-slack.sh
#
# Real-Slack smoke for the channels subsystem. Requires:
#   - $SLACK_BOT_TOKEN  (xoxb-...)
#   - $SLACK_APP_TOKEN  (xapp-...)
#   - $ANTHROPIC_API_KEY
#   - A reachable cluster with `oap init` already applied.
#
# Walks: apply Secret -> apply Channel -> wait Connected=True -> print
# "ready" -> wait for ENTER -> tear down. Use --keep to skip teardown.

set -euo pipefail

# Style helpers — match the numbered step / dimmed-command pattern used
# throughout this repo's smoke and test scripts.
DIM='\033[2m'
RED='\033[31m'
GREEN='\033[32m'
YELLOW='\033[33m'
NC='\033[0m'

step() { printf "\n=== step %d: %s ===\n" "$1" "$2"; }
run_() { printf "${DIM}\$ %s${NC}\n" "$*"; "$@"; }
ok()   { printf "${GREEN}✓${NC} %s\n" "$1"; }
warn() { printf "${YELLOW}!${NC} %s\n" "$1"; }
fail() { printf "${RED}✗${NC} %s\n" "$1"; exit 1; }

KEEP=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep) KEEP=true; shift ;;
    -h|--help)
      cat <<EOF
Usage: $0 [--keep]

Smoke-test the Slack channel adapter against a real Slack workspace.

Required env:
  SLACK_BOT_TOKEN    xoxb-... bot OAuth token
  SLACK_APP_TOKEN    xapp-... app-level token (connections:write scope)
  ANTHROPIC_API_KEY  Anthropic key for the smoke agent

Optional env:
  NAMESPACE          Kubernetes namespace to use (default: default)
  AGENTCLASS_FILE    Path to AgentClass manifest (default: examples/agentclass/no-tools.yaml)

Flags:
  --keep    Skip teardown on exit (leave Secret + Channel in the cluster).
  -h|--help Print this message and exit.
EOF
      exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

NAMESPACE="${NAMESPACE:-default}"
CHANNEL="slack-smoke"
SECRET="slack-smoke-creds"
AGENTCLASS="smoke-agent"

# The repo ships examples/agentclass/no-tools.yaml with metadata.name=smoke-agent.
# Override AGENTCLASS_FILE if you want a different manifest.
AGENTCLASS_FILE="${AGENTCLASS_FILE:-examples/agentclass/no-tools.yaml}"

cleanup() {
  if [[ "$KEEP" == "true" ]]; then
    warn "skipping cleanup (--keep)"
    return
  fi
  printf "\n=== cleanup ===\n"
  kubectl -n "$NAMESPACE" delete channel "$CHANNEL" --ignore-not-found || true
  kubectl -n "$NAMESPACE" delete secret "$SECRET" --ignore-not-found || true
  ok "cleanup done"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# step 0: verify environment
# ---------------------------------------------------------------------------
step 0 "verify environment"
[[ -n "${SLACK_BOT_TOKEN:-}" ]]   || fail "SLACK_BOT_TOKEN not set"
[[ -n "${SLACK_APP_TOKEN:-}" ]]   || fail "SLACK_APP_TOKEN not set"
[[ -n "${ANTHROPIC_API_KEY:-}" ]] || fail "ANTHROPIC_API_KEY not set (needed for the smoke agent)"
command -v oap     >/dev/null 2>&1 || fail "oap binary not in PATH — build with: go run github.com/magefile/mage build:oap"
command -v kubectl >/dev/null 2>&1 || fail "kubectl not in PATH"
ok "env ready"

# ---------------------------------------------------------------------------
# step 1: cluster + operator health
# ---------------------------------------------------------------------------
step 1 "cluster + operator health"
run_ oap check
ok "cluster reachable"

# ---------------------------------------------------------------------------
# step 2: ensure smoke AgentClass exists
# ---------------------------------------------------------------------------
step 2 "ensure AgentClass $AGENTCLASS"
if ! kubectl -n "$NAMESPACE" get agentclass "$AGENTCLASS" >/dev/null 2>&1; then
  if [[ ! -f "$AGENTCLASS_FILE" ]]; then
    fail "AgentClass '$AGENTCLASS' not found and '$AGENTCLASS_FILE' does not exist"
  fi
  # Substitute the API key into the manifest before applying so the runner
  # can actually call Anthropic.
  sed "s/REPLACE_ME/$ANTHROPIC_API_KEY/" "$AGENTCLASS_FILE" \
    | run_ kubectl -n "$NAMESPACE" apply -f -
else
  ok "AgentClass already present"
fi
ok "AgentClass ready"

# ---------------------------------------------------------------------------
# step 3: apply credentials Secret
# ---------------------------------------------------------------------------
step 3 "apply Secret $SECRET"
kubectl -n "$NAMESPACE" create secret generic "$SECRET" \
  --from-literal=bot-token="$SLACK_BOT_TOKEN" \
  --from-literal=app-token="$SLACK_APP_TOKEN" \
  --dry-run=client -o yaml | run_ kubectl apply -f -
ok "Secret applied"

# ---------------------------------------------------------------------------
# step 4: apply Channel CR
# ---------------------------------------------------------------------------
step 4 "apply Channel $CHANNEL"
run_ kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: Channel
metadata:
  name: $CHANNEL
  namespace: $NAMESPACE
spec:
  kind: slack
  agentClass: $AGENTCLASS
  sessionScope: auto
  credentialsRef:
    secretName: $SECRET
  slack:
    mode: socket
EOF
ok "Channel applied"

# ---------------------------------------------------------------------------
# step 5: wait for Connected=True (90 s)
# ---------------------------------------------------------------------------
step 5 "wait for Connected=True (90 s)"
status=""
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  status=$(kubectl -n "$NAMESPACE" get channel "$CHANNEL" \
    -o jsonpath='{.status.conditions[?(@.type=="Connected")].status}' 2>/dev/null || true)
  if [[ "$status" == "True" ]]; then
    ok "Connected=True"
    break
  fi
  sleep 2
done
if [[ "$status" != "True" ]]; then
  # Print the full status for debugging before failing.
  kubectl -n "$NAMESPACE" get channel "$CHANNEL" -o yaml || true
  fail "Channel did not reach Connected=True within 90 s"
fi

# ---------------------------------------------------------------------------
# step 6: ready — hand off to the human
# ---------------------------------------------------------------------------
step 6 "ready"
run_ oap channel show "$CHANNEL"
printf "\n"
printf "The bot is now listening on Slack.\n"
printf "  Try in Slack: invite the bot to a channel, then post: '@<bot> hello'\n"
printf "  Or DM the bot directly.\n"
printf "\n"
printf "Press ENTER to tear down (or Ctrl-C to exit + leave it running with --keep).\n"
read -r _
