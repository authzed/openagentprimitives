//go:build e2e

// join_deny_and_nonowner_test.go extends the multiplayer-join coverage in this
// package (see sessioninteract_grant_test.go for the approve-happy-path) with
// the two flows the join-approval bug reports were about:
//
//   - DENY: User A denies User B's join → B is blocked (is_denied written, no
//     interact granted), and B's stashed message is NOT replayed.
//
//   - NON-OWNER APPROVE: a user who is NOT the session owner clicks Approve on
//     B's request. Before the fix, an approver whose identity failed to
//     canonicalize to an owner was recast to a "deny" (the requester was told
//     they were rejected). The fix surfaces it as an authz failure instead —
//     no state change, the PendingRequester survives — so the REAL owner can
//     still approve and B's message is then processed. This is the end-to-end
//     analog of the "approve becomes a denial / wrong approver" reports; the
//     Slack-listener email-drop and the Slack decision-message rendering that
//     the reports quoted are covered by unit tests in pkg/channels/channelkinds/slack
//     and pkg/channels/channelsd/pipeline (the fake-kind harness bypasses the Slack
//     listener/renderer, so those specific surfaces can't be exercised here).
//
// Both reuse agentClassOverride from sessioninteract_grant_test.go, which sets
// authz.session.interactPermission to a group nobody is in, so any user other
// than the started_by routes through the permission_request flow.
//
// No real names: alice/bob/charlie @example.com per AGENTS.md.
package sessioninteract_grant_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// startJoinFixture boots the harness with the shared centerdot AgentClass
// override, primes the two declared MCP tools + LLM greeting rules, and drives
// turn 1 (alice starts the session). It returns the ready harness so each test
// can drive the join it needs. bobRule=true also registers a reply for bob's
// greeting (used by tests where bob's message is eventually processed).
func startJoinFixture(t *testing.T, bobRule bool) *e2e.Harness {
	t.Helper()
	h := e2e.Start(t, e2e.Options{
		AgentDir:       "../../../testdata/agent-centerdot-companies",
		ExtraManifests: []string{agentClassOverride},
		// Matches sessioninteract_grant_test.go: later turns may wait on a cold
		// runner respawn + a join round-trip; 60s absorbs suite contention.
		DefaultTimeout: 60 * time.Second,
	})
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []map[string]any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []map[string]any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	h.LLM.OnUserMessage("hi from alice").Reply(e2e.RespondToUser("hello alice"))
	if bobRule {
		h.LLM.OnUserMessage("hi from bob").Reply(e2e.RespondToUser("hello bob"))
	}
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	// Turn 1: alice starts the session (allowed as started_by).
	h.SendUserMessage("hi from alice", e2e.AsUser("alice@example.com"))
	h.ExpectAgentReply(e2e.Contains("hello alice"))
	return h
}

// theSession returns the single AgentSession the fixture created (bob joins
// alice's thread, so exactly one exists) and its "agentsession:<ns>/<name>"
// SpiceDB resource ref.
func theSession(t *testing.T, h *e2e.Harness) (spiceboxv1alpha1.AgentSession, string) {
	t.Helper()
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(context.Background(), &sessions), "list AgentSession CRs")
	require.Len(t, sessions.Items, 1, "exactly one AgentSession expected")
	sess := sessions.Items[0]
	return sess, "agentsession:" + sess.Namespace + "/" + sess.Name
}

// pendingHasBob reports whether bob is still an outstanding PendingRequester on
// the session — the deterministic signal for "the join decision hasn't cleared
// this request".
func pendingHasBob(t *testing.T, h *e2e.Harness, sessName, sessNS string) bool {
	t.Helper()
	var sess spiceboxv1alpha1.AgentSession
	if err := h.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: sessNS, Name: sessName}, &sess); err != nil {
		t.Logf("get session: %v", err)
		return false
	}
	for _, pr := range sess.Status.PendingRequesters {
		if pr.ExternalID == "bob@example.com" {
			return true
		}
	}
	return false
}

// TestCenterdot_SessionJoin_DenyBlocksJoiner: (a) alice starts, (b) bob posts,
// (c) join prompt, (d) alice DENIES, (e) bob is blocked — is_denied is written,
// interact is NOT granted, and bob's message is never replayed.
func TestCenterdot_SessionJoin_DenyBlocksJoiner(t *testing.T) {
	h := startJoinFixture(t, false) // no bob reply rule: bob must never be processed

	h.SendUserMessage("hi from bob", e2e.AsUser("bob@example.com"))
	join := h.ExpectSessionJoinPrompt(
		e2e.JoinFromRequester("bob@example.com"),
		e2e.JoinToStartedBy("alice@example.com"),
	)

	// Alice denies. HandleDecision writes the per-user denied relation, clears
	// the PendingRequester, and does NOT replay bob's stashed message.
	join.Deny(e2e.AsUser("alice@example.com"))

	sess, resourceRef := theSession(t, h)
	bobSubjectTyped, err := identity.FromExternal("fake", "", "bob@example.com", "bob@example.com").Subject()
	require.NoError(t, err)
	bobSubject := bobSubjectTyped

	// The deny is applied once the PendingRequester clears (the denied relation
	// is written before the status clear in HandleDecision).
	require.Eventually(t, func() bool {
		return !pendingHasBob(t, h, sess.Name, sess.Namespace)
	}, 30*time.Second, 200*time.Millisecond,
		"deny must clear bob's PendingRequester")

	// bob is explicitly blocked and was NOT granted interact.
	h.AssertSpiceDB(resourceRef, "is_denied", bobSubject, true)
	h.AssertSpiceDB(resourceRef, "interact", bobSubject, false)

	// Only alice's greeting rule may have fired; bob's message was never
	// replayed (no rule was registered for it).
	h.AssertAllRulesConsumed()
}

// TestCenterdot_SessionJoin_NonOwnerApproveRejected_OwnerApproveWorks:
// (a) alice starts, (b) bob posts, (c) join prompt, (d) a NON-OWNER (charlie)
// clicks Approve — which must be an authz FAILURE, not a fabricated denial:
// bob stays pending and is not granted. (d') the real owner (alice) then
// approves, and (e) bob's message is processed.
func TestCenterdot_SessionJoin_NonOwnerApproveRejected_OwnerApproveWorks(t *testing.T) {
	h := startJoinFixture(t, true) // bob IS eventually processed once alice approves

	h.SendUserMessage("hi from bob", e2e.AsUser("bob@example.com"))
	join := h.ExpectSessionJoinPrompt(
		e2e.JoinFromRequester("bob@example.com"),
		e2e.JoinToStartedBy("alice@example.com"),
	)

	sess, resourceRef := theSession(t, h)
	bobSubjectTyped, err := identity.FromExternal("fake", "", "bob@example.com", "bob@example.com").Subject()
	require.NoError(t, err)
	bobSubject := bobSubjectTyped

	// A non-owner attempts to approve. The owner-check fails; per the fix this
	// is surfaced as an error (NOT recast to a deny) and leaves the request
	// intact — bob must stay pending and must NOT be granted interact.
	join.Approve(e2e.AsUser("charlie@example.com"))
	require.Never(t, func() bool {
		return !pendingHasBob(t, h, sess.Name, sess.Namespace)
	}, 4*time.Second, 250*time.Millisecond,
		"a non-owner's Approve must not clear bob's PendingRequester (it is an authz failure, not a decision)")
	h.AssertSpiceDB(resourceRef, "interact", bobSubject, false)

	// The real owner can still approve — proof the request survived the
	// unauthorized click. bob's stashed message replays and the agent replies.
	join.Approve(e2e.AsUser("alice@example.com"))
	h.ExpectAgentReply(e2e.Contains("hello bob"))
	require.Eventually(t, func() bool {
		return !pendingHasBob(t, h, sess.Name, sess.Namespace)
	}, 30*time.Second, 200*time.Millisecond,
		"the owner's approve must clear bob's PendingRequester")
	h.AssertSpiceDB(resourceRef, "interact", bobSubject, true)

	h.AssertAllRulesConsumed()
}
