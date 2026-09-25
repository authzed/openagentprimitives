//go:build e2e

// Package sessioninteract_grant_test exercises the multiplayer-sessions
// flow: User A starts the session (becoming started_by), User B sends a
// follow-up, the channelsd pipeline emits a permission_request DM to
// User A, User A approves, User B's previously-stashed message replays
// through the inbound pipeline and the agent replies to it.
//
// The flow is end-to-end-functional in production via channelsd's
// pipeline.HandleDecision path, but the harness's session-join wiring
// is a separate piece of plumbing from T10's tool-approval wiring:
// different envelope kind (channelevents.KindPermissionRequest vs
// KindToolApprovalRequest), different pipeline handler (HandleDecision
// vs HandleToolApprovalDecision), different approver-resolution path
// (started_by vs ApproverSubject lookup against SpiceDB). T16 adds:
//
//   - The fake permission_request sub-channel sender's KindPermissionRequest
//     branch (Driver.SessionJoinPrompts queue).
//   - Harness NATS subscription on ap.session.*.*.in.permission_decision
//     routed to pl.HandleDecision.
//   - The SessionJoinApproval API on the harness — ExpectSessionJoinPrompt,
//     SessionJoinApproval.Approve/Deny, JoinFromRequester/JoinToStartedBy
//     predicates — mirroring T10's Approval API for the session-join
//     envelope shape.
//   - This scenario: a test-side AgentClass override (via Options.ExtraManifests)
//     setting authz.session.interactPermission so the second user routes through
//     the join-approval flow rather than being silently accepted.
//
// The override is a FULL AgentClass spec because the harness's
// applyManifestLabeled does Create-then-Update (whole-object replace,
// not strategic merge patch); a partial spec would lose the original
// systemPrompt/model/mcpServers/etc fields and the AgentClass would
// fail validation.
//
// No real names: alice@example.com and bob@example.com per AGENTS.md.
package sessioninteract_grant_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// agentClassOverride re-applies the centerdot-companies AgentClass with
// authz.session.interactPermission set so the multiplayer-join flow gates the
// second user through approval. The format must match
// `<type>:<id>#<relation>` per pkg/controllers/agentclass.sessionInteractPermissionRE;
// the value "group:eng#member" is not seeded in SpiceDB (no group:eng
// tuples exist), so any user other than alice (started_by) fails
// CheckInteract and routes through the permission_request flow.
const agentClassOverride = `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: centerdot-companies
  namespace: default
spec:
  displayName: CenterdotBot
  description: "E2E fixture agent. Do not deploy to a real cluster."
  authz:
    session:
      interactPermission: "group:eng#member"
    slots:
      - resourceType: crm_company
        description: "A CRM company record"
        permission: contact_access
  model:
    provider: test
    name: scripted
    apiKey:
      name: centerdot-placeholder
      key: api-key
  systemPrompt:
    inline: |
      You are a fixture agent for the agentprimitives e2e harness.
      Reply concisely to greetings.
  agentIdentity: centerdot-identity
  mcpServers:
    - name: centerdot
      ref: centerdot-companies
  budget:
    maxTurns: 10
    maxTokens: 50000
    maxDuration: 5m
`

func TestCenterdot_SessionInteract_JoinViaApproval(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       "../../../testdata/agent-centerdot-companies",
		ExtraManifests: []string{agentClassOverride},
		// Later turns wait on a COLD runner respawn (an earlier turn goes Idle
		// and the runner is torn down before the next SendUserMessage), which can
		// exceed the 10s default under suite contention (see contacts_owner_deny
		// for the full root-cause note). This scenario additionally interleaves a
		// join approval before the respawned turn and flaked once at 30s under a
		// full-suite run (2026-07-02: reply timed out with RunnerReady=False/
		// RunnerCreating, join already applied) — give it 60s.
		DefaultTimeout: 60 * time.Second,
	})

	// Pre-seed MCP for BOTH declared tools BEFORE WaitForAgentClassValid:
	// the MCPServer controller's tools/list probe + AgentClass
	// binding-coverage Check will trip with Valid=False reason=AllowlistDrift
	// if either declared tool is missing on the live MCP server. The
	// session-join flow itself doesn't invoke either tool — the LLM
	// only emits respond_to_user — but the AgentClass refuses to go
	// Valid until the declared MCP surface matches the live server.
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []map[string]any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []map[string]any{}}
	})

	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	// Bootstrap-written tuples don't affect this scenario (no resource
	// checks fire), but waiting on the bootstrap keeps the scenario
	// internally consistent with the other centerdot tests and avoids
	// a class-vs-bootstrap race surface if the fixture later grows.
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// LLM script. Two non-repeating UserMessage rules, one for each
	// user's distinct greeting text, so the second ExpectAgentReply can
	// match on the substring unique to bob's reply (it polls the full
	// Driver.Sent queue from index 0 each call; an identical reply text
	// for both turns would silently match alice's turn-1 reply and pass
	// before bob's reply even lands). The EndTurn rule on
	// respond_to_user's tool_result is repeating because it fires once
	// per turn.
	h.LLM.OnUserMessage("hi from alice").
		Reply(e2e.RespondToUser("hello alice"))
	h.LLM.OnUserMessage("hi from bob").
		Reply(e2e.RespondToUser("hello bob"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn()).Repeating()

	// Turn 1: alice starts the session. As started_by she's allowed
	// through CheckInteract on her own session (the pipeline writes
	// started_by on session create) — no approval expected.
	h.SendUserMessage("hi from alice", e2e.AsUser("alice@example.com"))
	h.ExpectAgentReply(e2e.Contains("hello alice"))

	// Turn 2: bob tries to join the same thread. He's not started_by,
	// he's not in group:eng (no SpiceDB tuples for that group exist),
	// and no per-user participant tuple has been written for him. So
	// CheckInteract returns false → handlePermissionDeny stashes his
	// message text on PendingRequesters and publishes a
	// permission_request envelope. The outbound relay routes it to the
	// fake permission_request sub-channel sender, where
	// ExpectSessionJoinPrompt drains it.
	h.SendUserMessage("hi from bob", e2e.AsUser("bob@example.com"))

	join := h.ExpectSessionJoinPrompt(
		e2e.JoinFromRequester("bob@example.com"),
		e2e.JoinToStartedBy("alice@example.com"),
	)

	// Alice approves. HandleDecision validates approver==started_by,
	// writes the per-user participant tuple for bob, clears the
	// PendingRequester, and replays bob's stashed "hi from bob"
	// through the inbound pipeline. The replay routes through the
	// runner (now that bob's CheckInteract passes via the freshly-
	// written participant tuple), the LLM rule fires, and
	// respond_to_user emits with "hello bob".
	join.Approve(e2e.AsUser("alice@example.com"))

	// The replayed inbound produces "hello bob" — distinct from alice's
	// turn-1 reply so the substring match unambiguously asserts the
	// replayed turn completed.
	h.ExpectAgentReply(e2e.Contains("hello bob"))

	// ── Per-turn authorship + channel-identity directory (multiplayer) ──
	//
	// Beyond the reply text, the feature under test (a) attributes the
	// RESUBMITTED turn to the JOINER (bob), not the session initiator
	// (alice), and (b) records bob's channel-native identity in the durable
	// UserIdentity directory. Assert both end-to-end against the real stack.
	ctx := context.Background()

	// The joiner's / initiator's canonical subjects. The fake channel sets
	// Email==ExternalID (see SendUserMessage's InboundEvent), so channelsd's
	// canonicalID/authorSubject resolve to base64url(email) and the teamScope
	// is irrelevant to the canonical — passing "" here matches the pipeline
	// exactly. Principal.Subject() returns the "user:<canonical>" string.
	joinerSubject, err := identity.FromExternal("fake", "", "bob@example.com", "bob@example.com").Subject()
	require.NoError(t, err)
	initiatorSubject, err := identity.FromExternal("fake", "", "alice@example.com", "alice@example.com").Subject()
	require.NoError(t, err)
	require.NotEqual(t, initiatorSubject, joinerSubject,
		"fixture sanity: joiner and initiator must canonicalize to distinct subjects")

	// bob joined alice's thread rather than starting a new one, so exactly
	// one AgentSession exists. Read its transcript scope directly from the
	// shared in-process memory facade (the same store the runner writes to).
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(ctx, &sessions), "list AgentSession CRs")
	require.Len(t, sessions.Items, 1, "bob joins alice's session; exactly one AgentSession expected")
	sess := sessions.Items[0]
	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}

	// Assertion 1: the resubmitted "user" turn carrying bob's text is authored
	// by bob's canonical subject — NOT alice's. channelsd stamps the per-turn
	// Author on the human "inbox" turn; the runner preserves it across the
	// inbox->user drain (loop.go drainInbox: Author: it.Author). Poll: the user
	// turn is written before the reply that ExpectAgentReply already matched,
	// but polling keeps this robust under suite contention.
	var joinerTurn memory.Turn
	require.Eventually(t, func() bool {
		turns, err := turn.ReadAll(memory.WithSystemApproval(ctx, "e2e-test"), h.Memory(), scope)
		if err != nil {
			t.Logf("read transcript turns: %v", err)
			return false
		}
		for _, tn := range turns {
			if tn.Role == "user" && turnHasText(tn, "hi from bob") {
				joinerTurn = tn
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond,
		"transcript must contain the resubmitted joiner (bob) user turn")

	assert.Equal(t, joinerSubject, joinerTurn.Author,
		"resubmitted user turn must be authored by the joiner (bob)")
	assert.NotEqual(t, initiatorSubject.String(), joinerTurn.Author.String(),
		"resubmitted user turn must NOT be attributed to the session initiator (alice)")

	// Assertion 2: a UserIdentity exists for bob's subject, carrying a
	// channelIdentities directory entry keyed (kind=fake, domain="",
	// externalID=bob@example.com). channelsd ensure-creates it on bob's first
	// inbound (before the interact deny) and upserts the entry. The fake kind
	// has no display name, so DisplayName is empty — expected and fine.
	uiName := useridentity.NameForSubject(joinerSubject)
	var ui spiceboxv1alpha1.UserIdentity
	require.Eventually(t, func() bool {
		return h.K8s.Get(ctx, client.ObjectKey{Name: uiName}, &ui) == nil
	}, 30*time.Second, 200*time.Millisecond,
		"UserIdentity %s must be ensure-created for the joiner subject", uiName)

	foundCI := false
	for _, ci := range ui.Status.ChannelIdentities {
		if ci.Kind == "fake" && ci.Domain == "" && ci.ExternalID == "bob@example.com" {
			foundCI = true
			break
		}
	}
	assert.True(t, foundCI,
		"UserIdentity %s must carry a (fake, \"\", bob@example.com) channelIdentities entry; got %+v",
		uiName, ui.Status.ChannelIdentities)

	// Every non-repeating rule must have fired exactly once. The two
	// OnUserMessage rules each fire once (turn 1 and the replayed
	// turn); the EndTurn rule is Repeating and excluded from this
	// check.
	h.LLM.AssertAllRulesConsumed()
}

// turnHasText reports whether any content block of tn contains substr.
func turnHasText(tn memory.Turn, substr string) bool {
	for _, b := range tn.Content {
		if strings.Contains(b.Text, substr) {
			return true
		}
	}
	return false
}
