#!/usr/bin/env bash
# Manual e2e smoke for slice 2 approval flow.
# Prereqs:
#   - kind/docker-desktop cluster with oap installed (mage build:oap + ./bin/oap init)
#   - SpiceDB running and reachable
#   - Slack app deployed with bot token
#   - An AgentClass with one tool whose Permission.StateImpact == external
#   - User has a fresh Slack thread to the bot
set -euo pipefail

echo "Step 1: in Slack, send a message that triggers the external tool"
read -p "Press enter once you see the approval prompt in Slack..."

echo "Step 2: click Approve in your ephemeral message"
read -p "Press enter once you see '✅ Approved by @you' in the thread..."

echo "Step 3: verify the agent reports the tool succeeded"
read -p "Press enter once the agent's reply lands..."

echo "Step 4: verify the grant tuple was deleted (external = one-shot)"
zed relationship read agentsession --consistency=fully-consistent 2>&1 | grep -v "^# " || echo "OK: no lingering grants visible"

echo "Step 5: clear pending tool grants check"
kubectl get agentsessions -A -o json | jq '.items[] | select(.status.pendingToolGrants | length > 0) | .metadata.name' && echo "warn: some sessions still have pending tool grants" || echo "OK: no pending tool grants"

echo "Done."
