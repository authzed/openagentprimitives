#!/usr/bin/env bash
# Manual e2e smoke for slice 3 entity binding.
# Prereqs:
#   - kind/docker-desktop cluster with oap installed (mage build:oap + ./bin/oap init)
#   - SpiceDB running, schema has github_repo with read permission
#   - product-manager AgentClass deployed with boundEntities (Task 16)
#   - User has interact + read on at least one of the defaults
#   - Slack thread to the bot
set -euo pipefail

echo "Step 1: in Slack, send a message that does NOT name any repo."
echo "        Expected: the agent operates on the AgentClass defaults."
read -p "Press enter once the agent responds..."

echo "Step 2: send 'now check status on foo/bar'."
echo "        Expected: foo/bar is extracted; if user has read perm, it binds."
echo "        If not, an approval prompt appears for the approverSubject."
read -p "Press enter once the agent responds (or approval lands)..."

echo "Step 3: verify the session's bindings"
SESS=$(kubectl get agentsessions -A -o json | jq -r '.items[] | select(.status.phase != "Failed") | .metadata.name' | head -1)
kubectl describe agentsession -A "$SESS" | grep -A 20 "Bound Entities:"

echo "Done."
