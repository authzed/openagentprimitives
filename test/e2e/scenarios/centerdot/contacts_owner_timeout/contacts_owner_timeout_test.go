//go:build e2e

// Package contacts_owner_timeout_test drives the tool_call approval TIMEOUT
// subject-routing path end-to-end — the one branch of the tool-approval flow
// that had unit coverage on both ends but no e2e exercise of the NATS
// round-trip between them.
//
// Setup mirrors the canonical contacts_owner_approval scenario (two-turn
// centerdot flow, owner-derived approver) with ONE change: an ExtraManifests
// override sets a short authz.approvalTimeout on the AgentClass so the runner's
// approval-WAIT deadline fires within the test budget instead of the 10-minute
// default.
//
// Flow:
//
//	Turn 1 — "companies …": list_companies passes the wildcard Check and
//	  JIT-writes owner_ref for each company (so the turn-2 ApproverSubject
//	  lookup resolves owner-1). No approval.
//
//	Turn 2 — "contacts on Acme": Check on crm_company:acme-id#contact_access
//	  fails → the runner publishes a tool_approval_request; channelsd's
//	  HandleToolApprovalRequest patches PendingToolGrants + the
//	  ToolApprovalPending condition and forwards the prompt (captured by
//	  ExpectApprovalPrompt). The approver NEVER clicks. After ~3s the runner's
//	  AwaitDecision ctx-deadline elapses; host_approval.go's publishTimeoutApplied
//	  fans a deny-only tool_approval_applied envelope to BOTH the IN subject
//	  (channelsd's HandleToolApprovalApplied clears PendingToolGrants + the
//	  condition) and the OUT subject. The gated MCP tool is NEVER executed, and
//	  because a tool_call timeout is a STICKY-DENY (not a Halt), the turn
//	  continues: the runner feeds a SYSTEM_TIMEOUT error tool_result back to the
//	  LLM, which reports the expiry to the user.
//
// Assertions (the point of the scenario):
//  1. The tool_approval_request round-tripped (ExpectApprovalPrompt) — proving
//     PendingToolGrants was set, since the pipeline patches it before
//     forwarding the prompt to the sender.
//  2. After the timeout, PendingToolGrants is empty AND ToolApprovalPending is
//     False — proving the timeout Applied envelope routed through channelsd's
//     HandleToolApprovalApplied and cleared the channel-facing surface (rather
//     than stranding it).
//  3. list_contacts_for_company was NEVER executed — a lapsed approval never
//     runs the gated tool (deny-only).
//  4. The turn continues and the agent replies to the user (the timeout is a
//     graceful deny, NOT a RunnerCrashed session failure).
//
// No real names — Acme / Beta / Centerdot / owner-1 / user@example.com are all
// fictional per AGENTS.md.
package contacts_owner_timeout_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// contactPayloadSentinel mirrors contacts_owner_deny's sentinel: a
// distinctive, obviously-fake identifier (no real names, per AGENTS.md) that
// the gated MCP tool returns instead of a plausible-looking address. It
// exists so a fail-open regression that actually runs the tool is provable
// from the channel-visible reply alone, not just from the MCP-call-count
// instrumentation this scenario already had.
const contactPayloadSentinel = "CONTACT-PAYLOAD-MUST-NOT-APPEAR"

func TestCenterdot_ContactsApprovalTimeout_ClearsPendingGrant(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: "../../../testdata/agent-centerdot-companies",
		// Turn 2's ExpectApprovalPrompt must outwait the full turn-2 cold runner
		// respawn (turn 1 goes Idle/AgentWorkComplete and the runner is torn down
		// before turn 2's SendUserMessage): the prompt can't be published until a
		// fresh runner is up, reaches the gated tool_use, and fails the Check. That
		// respawn is the dominant, variable cost, and ExpectApprovalPrompt charges
		// it against DefaultTimeout — nominal ~14s but 40s+ under suite contention
		// (see the regression note in test/e2e/inprocess_runner_factory.go). The
		// prompt log is append-only, so a generous budget cannot "miss" the prompt;
		// 30s straddled the flake boundary. 60s matches contacts_multiturn (which
		// does MORE turns) and this scenario does MORE post-prompt work than the
		// 30s siblings (approval-WAIT deadline + timeout-applied round-trip +
		// PendingToolGrants clear + continuation reply). Residual: under
		// pathological full-parallel `mage test:e2e` load the whole centerdot
		// family can still thundering-herd; run e2e serialized (-p 1) for a
		// reliable signal.
		DefaultTimeout: 60 * time.Second,
		// Override the centerdot AgentClass to shorten the approval-WAIT
		// deadline so the timeout path is reachable inside the test budget.
		ExtraManifests: []string{approvalTimeoutOverride("3s")},
	})

	// Both declared tools need MCP handlers for AgentClass Valid=True (the
	// MCPServer controller's tools/list probe gates on it). list_contacts_for_company
	// is registered so the AgentClass converges, but the timeout short-circuits
	// dispatch before Execute — it must never actually be invoked.
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{
			"results": []map[string]any{
				{"id": "acme-id", "name": "Acme", "ownerId": "owner-1"},
				{"id": "beta-id", "name": "Beta", "ownerId": "owner-2"},
				{"id": "centerdot-id", "name": "Centerdot", "ownerId": "owner-1"},
			},
		}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		// A lapsed approval must never run the gated tool. A distinctive
		// sentinel (not a plausible-looking address like "alice@acme.com")
		// makes a regression loud two ways: contactsCallCount below catches
		// the tool having executed at all, and the NotContains assertion on
		// the final reply catches the sentinel leaking into the channel even
		// if some future change routes the leak through a different path
		// than a direct Execute call.
		return map[string]any{"results": []map[string]any{{"email": contactPayloadSentinel + "@acme.com"}}}
	})

	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// LLM script. Turn 2's tool_use fires once; the runner parks on the approval
	// and times out. A tool_call approval timeout is a STICKY-DENY (not a Halt):
	// the runner feeds a SYSTEM_TIMEOUT error tool_result back to the LLM and the
	// turn CONTINUES, so turn 2 needs a tool_result rule that responds to the
	// user (the agent reports the expiry rather than the session crashing).
	h.LLM.OnUserMessage("companies created in the last 2 weeks").
		Reply(e2e.ToolUse("centerdot_list_companies", map[string]any{
			"operation_id": "op-t1",
			"_reason":      "user asked for recent companies",
			"args":         map[string]any{"sinceDays": 14},
		}))
	h.LLM.OnToolResult("centerdot_list_companies", e2e.AnyResult()).
		Reply(e2e.RespondToUser("Found 3 companies: Acme, Beta, Centerdot"))
	h.LLM.OnUserMessage("contacts on Acme").
		Reply(e2e.ToolUse("centerdot_list_contacts_for_company", map[string]any{
			"operation_id": "op-t2",
			"_reason":      "user asked for contacts on Acme",
			"args":         map[string]any{"companyId": "acme-id"},
		}))
	// The gated tool call times out → the executor returns a SYSTEM_TIMEOUT deny
	// tool_result (the tool itself never executes) → the agent tells the user the
	// request expired. This is the sticky-deny continuation the fix guarantees.
	//
	// Deliberately left as AnyResult() rather than tightened to
	// ResultContains("SYSTEM_TIMEOUT") (contrast contacts_owner_deny, which
	// tightens on SYSTEM_DECISION_DENIED): this exact scenario is the
	// documented contention-flaky member of the centerdot family (see
	// memory/reference_e2e_contacts_owner_timeout_flake.md) and
	// approval_outcome.go's approvalFailureContent has a third branch,
	// SYSTEM_APPROVAL_ERROR, for when Orchestrator.Await's underlying publish
	// itself fails — plausible under the exact machine contention this family
	// already flakes on. Pinning the rule to SYSTEM_TIMEOUT would turn that
	// legitimate (if rare) variance into a hard ScriptedLLM.Send failure
	// instead of the graceful-deny content the test doesn't actually care
	// about the wording of. The security property this scenario exists to
	// prove — the gated tool never ran — is instead pinned by the sentinel
	// payload below: contactsCallCount==0 and the reply NotContains check
	// catch a fail-open regression regardless of which deny-shaped text the
	// runner produced.
	h.LLM.OnToolResult("centerdot_list_contacts_for_company", e2e.AnyResult()).
		Reply(e2e.RespondToUser("The approval request for the contacts expired without a response, so I couldn't retrieve them."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn()).Repeating()

	// Turn 1: companies. No approval — seeds owner_ref for acme.
	h.SendUserMessage("companies created in the last 2 weeks", e2e.AsUser("user@example.com"))
	h.ExpectAgentReply(e2e.Contains("Acme", "Beta", "Centerdot"))

	// NO co-owner seeding (see contacts_owner_approval): owner-1 is a real
	// approver purely as acme-id's owner via the turn-1 owner_ref JIT-write
	// — we simply never click.

	// Turn 2: contacts on Acme → approval prompt. Capturing it proves the
	// pipeline set PendingToolGrants (patched before the prompt is forwarded).
	h.SendUserMessage("contacts on Acme", e2e.AsUser("user@example.com"))
	prompt := h.ExpectApprovalPrompt(
		e2e.ForTool("centerdot_list_contacts_for_company"),
		e2e.ForResource("crm_company:acme-id"),
	)
	require.NotNil(t, prompt, "tool_approval prompt captured (PendingToolGrants was set)")

	// Do NOT approve. The runner's ~3s approval-WAIT deadline elapses,
	// publishTimeoutApplied fans a deny-only Applied envelope to IN+OUT, and
	// channelsd's HandleToolApprovalApplied clears the pending queue + condition.
	// Poll (generously past the 3s deadline) for the cleared state.
	requireClearedPendingApproval(t, h, 20*time.Second)

	// The turn CONTINUES after the sticky-deny: the agent reports the expiry to
	// the user rather than the session crashing (RunnerCrashed) — the whole point
	// of treating a user-facing approval timeout as a graceful deny.
	h.ExpectAgentReply(e2e.Contains("expired"))

	// Mirror of AnyResult()'s blind spot, closed two independent ways (see
	// contacts_owner_deny's equivalent block for the fuller rationale):
	//
	// 1. assertContactSentinelNeverObserved scans every request the
	//    ScriptedLLM actually saw. Checking the channel-visible reply text
	//    instead would be vacuous here: rule 3's Reply(...) above is a fixed
	//    canned string ("...expired without a response...") that never
	//    echoes the tool's real payload, so "expired" would still appear
	//    whether the tool ran or not — only the tool_result CONTENT the LLM
	//    was fed varies with what actually happened.
	// 2. contactsCallCount is the direct, LLM-independent proof: the gated
	//    tool was never invoked at all.
	assertContactSentinelNeverObserved(t, h)
	assert.Equal(t, 0, contactsCallCount(t, h),
		"list_contacts_for_company must NOT execute on an approval timeout")

	// Turn 2 is sent right as turn 1's runner idles, so this scenario rides the
	// same strand window as the rest of the centerdot family: if the wake were
	// dropped, "contacts on Acme" would sit undrained and the approval prompt
	// above would simply never arrive.
	h.AssertNoStrandedInbox()
}

// requireClearedPendingApproval polls the single AgentSession until its
// PendingInteractions queue is empty AND the ToolApprovalPending condition is
// not True (False or absent), failing if it never converges. tool_approval now
// parks on the generic PendingInteractions list, so the durable witness that the
// runner's timeout Applied envelope routed through channelsd (clearing the
// channel-facing surface) is that list draining.
func requireClearedPendingApproval(t *testing.T, h *e2e.Harness, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var (
		lastPending int
		lastCond    metav1.ConditionStatus = "<absent>"
	)
	for time.Now().Before(deadline) {
		sess := singleSession(t, h)
		lastPending = len(sess.Status.PendingInteractions)
		cond := meta.FindStatusCondition(sess.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionToolApprovalPending)
		condStatus := metav1.ConditionStatus("<absent>")
		if cond != nil {
			condStatus = cond.Status
		}
		lastCond = condStatus
		if lastPending == 0 && condStatus != metav1.ConditionTrue {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("approval-timeout cleanup never converged: PendingInteractions=%d (want 0), "+
		"ToolApprovalPending=%q (want != True) after %s", lastPending, lastCond, timeout)
}

// contactsCallCount returns how many times the gated MCP tool
// (list_contacts_for_company) was invoked on the in-process MCP stub.
func contactsCallCount(t *testing.T, h *e2e.Harness) int {
	t.Helper()
	n := 0
	for _, c := range h.MCP.Calls() {
		if c.Name == "list_contacts_for_company" {
			n++
		}
	}
	return n
}

// assertContactSentinelNeverObserved fails if contactPayloadSentinel shows up
// anywhere in any request the ScriptedLLM was sent — system prompt, message
// text, tool_use args, or tool_result content. Mirrors secret_output_test.go's
// assertNoLeak / test/e2e's established canary-scan idiom and
// contacts_owner_deny's identically-named helper.
func assertContactSentinelNeverObserved(t *testing.T, h *e2e.Harness) {
	t.Helper()
	reqs := h.LLM.Requests()
	require.NotEmpty(t, reqs, "scripted LLM must have observed at least one request")
	for ri, req := range reqs {
		for si, sb := range req.System {
			assertNoContactLeak(t, sb.Text, "request[%d].System[%d]", ri, si)
		}
		for mi, m := range req.Messages {
			for ci, cb := range m.Content {
				assertNoContactLeak(t, cb.Text, "request[%d].Messages[%d].Content[%d].Text", ri, mi, ci)
				if cb.ToolUse != nil {
					assertNoContactLeak(t, string(cb.ToolUse.Input), "request[%d].Messages[%d].Content[%d].ToolUse.Input", ri, mi, ci)
				}
				if cb.ToolResult != nil {
					assertNoContactLeak(t, cb.ToolResult.Content, "request[%d].Messages[%d].Content[%d].ToolResult.Content", ri, mi, ci)
				}
			}
		}
	}
}

// assertNoContactLeak fails (collecting, not aborting) if s contains the
// gated tool's sentinel payload.
func assertNoContactLeak(t *testing.T, s, format string, args ...any) {
	t.Helper()
	where := fmt.Sprintf(format, args...)
	assert.NotContains(t, s, contactPayloadSentinel, "gated tool's sentinel payload leaked into %s", where)
}

// singleSession returns the one AgentSession CR in the harness namespace.
func singleSession(t *testing.T, h *e2e.Harness) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(context.Background(), &sessions), "list AgentSessions")
	require.Len(t, sessions.Items, 1, "expected exactly one AgentSession")
	return &sessions.Items[0]
}

// approvalTimeoutOverride returns a full-spec AgentClass manifest identical to
// testdata/agent-centerdot-companies/03-agent.yaml but with a class-level
// authz.approvalTimeout added. Applied via ExtraManifests (Update after the
// AgentDir apply), it replaces the base AgentClass so the tool-call approval
// wait sources this short deadline. The per-tool enforceMode:always on
// list_contacts_for_company still routes the deny to approval regardless of the
// (unset) authz.toolCalls.mode, so approval behavior is unchanged except the
// wait length.
func approvalTimeoutOverride(timeout string) string {
	return `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: centerdot-companies
  namespace: default
spec:
  displayName: CenterdotBot
  description: "E2E fixture agent. Do not deploy to a real cluster."
  model:
    provider: test
    name: scripted
    apiKey:
      name: centerdot-placeholder
      key: api-key
  systemPrompt:
    inline: |
      You are a fixture agent for the agentprimitives e2e harness.
      You operate on companies and contacts. Available tools:
        - centerdot_list_companies(sinceDays): list companies created recently
        - centerdot_list_contacts_for_company(companyId): list contacts; gated by owner approval
      When the user asks about contacts, call centerdot_list_contacts_for_company
      with the company id from the prior centerdot_list_companies result.
  agentIdentity: centerdot-identity
  mcpServers:
    - name: centerdot
      ref: centerdot-companies
  authz:
    slots:
      - resourceType: crm_company
        description: "A CRM company record"
        permission: contact_access
    approvalTimeout: ` + timeout + `
  budget:
    maxTurns: 10
    maxTokens: 50000
    maxDuration: 5m
`
}
