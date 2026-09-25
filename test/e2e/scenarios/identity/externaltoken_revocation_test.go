//go:build e2e

// Package e2e_test carries the externaltoken token-use authorization
// end-to-end coverage: the durable, per-call SpiceDB use_token check the
// operator's reconcileCredentialGrants (grant writer) and the runner's MCP
// dispatcher (grant checker) implement together (see
// pkg/platform/identity/externaltoken, pkg/controllers/agentsession/credential_grants.go,
// pkg/agent/tool/mcp/dispatch.go).
//
// TestExternalToken_MidSessionRevocation_IsSurgical is the primary scenario:
// a live MCP credential works, is then revoked out from under a running
// session (removed from the AgentIdentity), and the very next call on that
// credential is denied — WITHOUT failing the session. This proves the full
// declarative-revocation chain in one process:
//
//	edit AgentIdentity (drop the credential)
//	  → AgentSession reconciler's AgentIdentity watch (mapAgentIdentityToSessions,
//	    registered unconditionally by agentsessionctrl.Reconciler.SetupWithManager)
//	    re-enqueues the (non-terminal) session
//	  → reconcileCredentialGrants recomputes the desired grant set; the
//	    credential is gone, so the credential's externaltoken grant is no
//	    longer desired → applyGrantDiff calls DeleteAuthorizedToken
//	  → the next MCP call's fully-consistent use_token check (dispatch.go,
//	    wired via MCPTool.SetUseTokenGate) sees the deleted grant and denies
//	  → the tool call returns IsError; the session survives (surgical).
//
// This exercises harness wiring that did not exist before this test:
// Options.WithTokenAuthz opts the AgentSession reconciler's
// TokenGranter/TokenChecker and the in-process runner factory's
// MCPTool.SetUseTokenGate into the SAME opt-in flag (see harness.go /
// inprocess_runner_factory.go), mirroring internal/cmd/operator/main.go +
// internal/cmd/runner/main.go's production wiring. Off by default so the many
// existing scenario tests — authored before this feature and never
// exercising it — are unaffected.
//
// A second scenario (SpiceDB made unreachable mid-session → the session
// itself fails with ReasonAgentSessionTokenAuthzUnavailable) is deliberately
// NOT included here: the in-process harness has no seam to swap a broken
// TokenChecker into an ALREADY-RUNNING session's MCPTool (the gate is frozen
// at tool-synthesis time, once, at session Start), and the only way to
// simulate "SpiceDB unreachable" without such a seam would be severing the
// harness's single shared *spicedb.Client — which every OTHER SpiceDB-backed
// facility in the same session (relwrites, approvals, the pipeline's own
// checks) also depends on, making the failure impossible to attribute
// cleanly to the token-authz path. That fail-closed path is already proven,
// including the real AgentSession.Status.Phase/FailureReason write:
//   - pkg/agent/tool/mcp/dispatch_test.go:TestExecute_UseTokenGate
//     ("indeterminate (false,err): IsError AND FailSession invoked") — the
//     MCP dispatch.go logic itself.
//   - pkg/controllers/toolcall/checktoken_test.go:
//     TestCheckTokenUse_Indeterminate_FailsBothToolCallAndSession — the
//     sandbox-path sibling, asserting the REAL reconciler write against a
//     fake client: AgentSession.Status.Phase == Failed AND
//     FailureReason == ReasonAgentSessionTokenAuthzUnavailable, plus the
//     matching Failed condition on both the ToolCall and the AgentSession.
//
// No real names: "owner-1", "acme-id" etc. are the existing centerdot
// fixture's fictional identifiers (see testdata/agent-centerdot-companies).
package identity_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
	"github.com/authzed/openagentprimitives/test/e2e"
)

func TestExternalToken_MidSessionRevocation_IsSurgical(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-centerdot-companies"),
		WithTokenAuthz: true,
		DefaultTimeout: 30 * time.Second,
		// AwaitIdleTTL keeps the SAME runner loop parked in-process across
		// both turns (see await_inprocess_resume_test.go) instead of the
		// default idle-exit-then-respawn cycle. Load-bearing here: a respawn
		// between turns would re-run buildMCPTools against the (soon to be
		// credential-less) identity and hard-fail MCP tool synthesis itself —
		// masking the use_token deny this test targets. Parking also keeps
		// AgentSession.Status.Phase at Running (never Idle) between turns,
		// which matters because the operator's 4c idle-sleep/archive
		// short-circuit (controller.go) returns BEFORE reaching
		// reconcileCredentialGrants whenever Phase==Idle and there's no fresh
		// wake annotation — exactly the state an AgentIdentity-watch-triggered
		// reconcile (not a new inbound) would otherwise land in, silently
		// skipping the very grant-delete this test polls for.
		AwaitIdleTTL: 30 * time.Second,
	})

	// Both declared tools need MCP handlers so the MCPServer controller's
	// tools/list probe passes AgentClass binding coverage (Valid=True).
	// list_contacts_for_company is never dispatched in this scenario.
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{
			"results": []map[string]any{
				{"id": "acme-id", "name": "Acme", "ownerId": "owner-1"},
			},
		}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})

	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// Turn 1: happy path — proves the credential AND its externaltoken grant
	// both work before anything is revoked.
	h.LLM.OnUserMessage("companies created in the last 2 weeks").
		Reply(e2e.ToolUse("centerdot_list_companies", map[string]any{
			"operation_id": "op-1",
			"_reason":      "user asked for recent companies",
			"args":         map[string]any{"sinceDays": 14},
		}))
	h.LLM.OnToolResult("centerdot_list_companies", e2e.AnyResult()).
		Reply(e2e.RespondToUser("Found 1 company: Acme"))

	// Turn 2 (post-revoke): the SAME tool call now denies at the use_token
	// gate BEFORE the request ever reaches the MCP server — the dispatcher's
	// per-call check runs ahead of probe.CallTool (see dispatch.go). The
	// second OnToolResult rule below only fires once the first is consumed
	// (turn 1), so it's the one that sees turn 2's deny content.
	h.LLM.OnUserMessage("companies again please").
		Reply(e2e.ToolUse("centerdot_list_companies", map[string]any{
			"operation_id": "op-2",
			"_reason":      "user asked again after revoke",
			"args":         map[string]any{"sinceDays": 14},
		}))
	h.LLM.OnToolResult("centerdot_list_companies", e2e.ResultMatches(func(v any) bool {
		s, ok := v.(string)
		return ok && strings.Contains(s, "revoked")
	})).Reply(e2e.RespondToUser("Access to your companies data has been revoked."))

	// Every respond_to_user (both turns) is followed by await_user_message —
	// NOT EndTurn — so the runner parks in-process (AwaitIdleTTL above)
	// instead of idle-exiting. Repeating: this rule fires after both turns.
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("await_user_message", map[string]any{})).Repeating()

	h.SendUserMessage("companies created in the last 2 weeks")
	h.ExpectAgentReply(e2e.Contains("Acme"))

	ctx := context.Background()
	ns, name := e2e.WaitForAnySession(t, ctx, h)

	// Confirm the park landed (Phase==Running, not Idle) before proceeding —
	// see the AwaitIdleTTL comment above for why this matters to the grant
	// reconcile that follows.
	requireSessionRunning(t, h, ns, name)

	// The externaltoken credID for the fixture's static MCP bearer credential
	// (testdata/agent-centerdot-companies/01-identity.yaml): AgentIdentity
	// "centerdot-identity" credential "centerdot-mcp-bearer" ->
	// Secret centerdot-mcp-token[access_token]. Mirrors credresolve.SourceFor's
	// static-credential derivation (id.StaticProjection is nil in agent mode,
	// so Namespace = the AgentIdentity's own namespace = the session namespace
	// here) — the SAME Source reconcileCredentialGrants derives, so the credID
	// is guaranteed byte-identical to what the operator actually granted.
	credID := externaltoken.CredID(spiceboxv1alpha1.CredentialSource{
		Type: "static", Namespace: ns, Name: "centerdot-mcp-token", Key: "access_token",
	})

	// Sanity: turn 1 could only have succeeded because this grant already
	// existed — confirm it directly rather than inferring it from the reply.
	// Synchronous pre-check (the grant was written before the session ever
	// became reachable) — the short deadline is fine here.
	requireGrantPresence(t, h, ns, name, credID, true, 15*time.Second)

	// Revoke: drop the credential from the AgentIdentity the class's runtime
	// identity resolves against. This is the production revocation trigger —
	// no direct SpiceDB write from the test.
	var ident spiceboxv1alpha1.AgentIdentity
	require.NoError(t, h.K8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "centerdot-identity"}, &ident),
		"get AgentIdentity centerdot-identity")
	ident.Spec.Credentials = nil
	require.NoError(t, h.K8s.Update(ctx, &ident),
		"remove centerdot-mcp-bearer from centerdot-identity")

	// Poll until the operator's AgentIdentity-watch-triggered reconcile has
	// actually deleted the grant. Required: the next tool call must not race
	// a reconcile that hasn't landed yet, or it would see the stale (still
	// present) grant and wrongly succeed. CheckUseToken is fully-consistent,
	// so once the DELETE has landed the very next check is guaranteed to see
	// it gone — the race is entirely on this write side, not the read side.
	//
	// The same deadline as the presence pre-check above. This poll waits on
	// the async revoke -> AgentIdentity-watch -> reconcile -> grant-diff ->
	// SpiceDB-delete chain, but that chain no longer races the OTHER effect of
	// the same write: removing the credential also drives the AgentIdentity,
	// and with it the AgentClass, to Valid=False. The session reconcile runs
	// its revocation sweep above the AgentClass gate, so whichever of the two
	// lands first, the delete still happens on that pass.
	//
	// Treat a timeout here as a real regression in that reachability, NOT as a
	// slow machine to be papered over by raising the deadline. The failure mode
	// this guards is binary: when the diff is skipped nothing re-enqueues the
	// session, so the grant never arrives at all and no deadline would help.
	requireGrantPresence(t, h, ns, name, credID, false, 15*time.Second)

	// Turn 2: same tool, now denied — surgical (the session must survive).
	h.SendUserMessage("companies again please")
	h.ExpectAgentReply(e2e.Contains("revoked"))

	h.LLM.AssertAllRulesConsumed()

	// The deny must happen at the use_token gate, strictly before the
	// dispatcher ever sends the request upstream: the MCP stub must have
	// recorded exactly the one (turn-1) call, never a second.
	assert.Len(t, h.MCP.Calls(), 1,
		"turn 2's call must be denied before it ever reaches the MCP server")

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sess))
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sess.Status.Phase,
		"a definitive token-use deny must be surgical — the session must NOT fail")
	assert.Empty(t, sess.Status.FailureReason,
		"no FailureReason should be set on a surgical (call-level, not session-level) deny")
}

// requireSessionRunning polls until the AgentSession reaches
// Phase==Running (parked in await_user_message, per AwaitIdleTTL), or fails
// the test after 30s. Mirrors await_inprocess_resume_test.go's
// waitForSessionRunning (package e2e, unavailable to this package_test file).
func requireSessionRunning(t *testing.T, h *e2e.Harness, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		var sess spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &sess); err == nil {
			last = sess.Status.Phase
			if last == spiceboxv1alpha1.AgentSessionPhaseRunning {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("AgentSession %s/%s never reached Phase=Running within 30s (last observed: %q)", ns, name, last)
}

// requireGrantPresence polls (h.SpiceDB) until the session's externaltoken
// authorized_token grant for credID matches `want` (present/absent), or
// fails the test after `timeout`. Fully consistent per ListAuthorizedTokens'
// own contract, so a positive result is authoritative the moment it's
// observed — the loop exists only to absorb the reconcile's own latency,
// not any SpiceDB read-consistency lag. Callers pick `timeout` per what
// they're actually waiting on — see the two call sites' comments.
func requireGrantPresence(t *testing.T, h *e2e.Harness, ns, name, credID string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastCredIDs []string
	for time.Now().Before(deadline) {
		grants, err := h.SpiceDB.ListAuthorizedTokens(context.Background(), ns, name)
		require.NoError(t, err, "ListAuthorizedTokens")
		lastCredIDs = lastCredIDs[:0]
		found := false
		for _, g := range grants {
			lastCredIDs = append(lastCredIDs, g.CredID)
			if g.CredID == credID {
				found = true
			}
		}
		if found == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("externaltoken grant %s presence != %v within %s (last observed grant credIDs: %v)",
		credID, want, timeout, lastCredIDs)
}
