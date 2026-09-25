//go:build e2e

// Package contacts_owner_approval_test is THE canonical end-to-end
// scenario the e2e framework was built to validate. Two-turn flow:
//
//	Turn 1 — "companies created in the last 2 weeks"
//	  user → LLM emits centerdot_list_companies(sinceDays=14)
//	    → MCP stub returns 3 companies. list_companies is passthrough
//	      (ungated), so no Check runs and no approval is needed.
//	    → writesRelationships JITs, per company in the result:
//	      crm_company:<id>#owner_ref@crm_owner:<id>  (who may approve) and
//	      crm_company:<id>#any_user@user:*           (the wildcard leaf the
//	      post-approval grant walk resolves through).
//	    → LLM emits respond_to_user("Found 3 companies: …")
//
//	Turn 2 — "contacts on Acme"
//	  user → LLM emits centerdot_list_contacts_for_company(companyId="acme-id")
//	    → The tool's Check sets routeViaSessionGrant, so the direct
//	      crm_company:acme-id#contact_access@user:<requester> check is
//	      BYPASSED and the only path to allow is
//	      agentsession:<sess>#check_contact_access_crm_company. No grant
//	      tuple exists yet, so it DENIES. (This is why the wildcard
//	      any_user tuple written in turn 1 grants nothing on its own.)
//	    → enforceMode:always on the tool routes the deny to approval.
//	    → Runner publishes tool_approval_request envelope on NATS.
//	    → Pipeline patches AgentSession.status.pendingToolGrants and
//	      forwards to the fake permission_request sub-channel sender;
//	      ExpectApprovalPrompt observes the captured envelope.
//	    → Approval.Approve(AsUser("owner-1@example.com")):
//	      - publishes tool_approval_decision on the right IN subject
//	      - pipeline LookupSubjects validates clicker ∈ ApproverSubject
//	        (crm_company:acme-id#owner resolves to owner-1@example.com
//	        via the turn-1 JIT-written owner_ref tuple chained to the
//	        bootstrap's crm_owner:owner-1#user@user:owner-1@example.com)
//	      - writes the session-scoped grant tuple to SpiceDB
//	      - publishes tool_approval_applied; runner resumes the paused
//	        dispatch.
//	    → The runner's post-approve re-check now RESOLVES: the grant tuple
//	      supplies the agentsession→crm_company hop and turn 1's
//	      any_user@user:* tuple satisfies the arrow's contact_access leaf
//	      for the original requester. This is the pair the whole
//	      three-part shape exists for — without the wildcard leaf the
//	      approved call is denied anyway, fail-closed and silently.
//	    → MCP stub returns contacts; LLM emits respond_to_user("Acme
//	      contacts: alice@acme.com, bob@acme.com").
//	    → ExpectAgentReply matches.
//
// This is the integration of:
//
//	T1 (harness skeleton)  T2 (ScriptedLLM)  T3 (centerdot fixture)
//	T4 (MCPStub)           T5 (in-process runner factory)
//	T6/T7/T8 (controllers) T9 (channelsd pipeline + outbound relay)
//	T10 (fake channel kind + permission_request sub-channel sender)
//	T11 (conversation API) T12 (approval API) T13 (no-approval baseline)
//
// If this test passes, the framework's load-bearing claim — "you can
// write a multi-turn permission-gated scenario in <100 lines" — is
// validated. If it times out or panics, dumpState() prints the LLM
// requests served, the channel outbound queue, the captured approval
// prompts, the active session's conditions, and any session-scoped
// SpiceDB tuples — that's almost always enough to find the gap without
// reaching for a debugger.
//
// No real names: Acme / Beta / Centerdot / owner-1 / alice@acme.com etc
// are all fictional per AGENTS.md.
package contacts_owner_approval_test

import (
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/test/e2e"
)

func TestCenterdot_ContactsRequireOwnerApproval(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: "../../../testdata/agent-centerdot-companies",
		// Turn 2's approval prompt waits on a COLD runner respawn (turn 1 goes
		// Idle and the runner is torn down before turn 2's SendUserMessage),
		// which can exceed the 10s default under suite contention. Match the 30s
		// headroom every other multi-turn e2e scenario uses (see
		// contacts_owner_deny for the full root-cause note).
		DefaultTimeout: 30 * time.Second,
	})

	// Seed MCP backing for BOTH declared tools BEFORE
	// WaitForAgentClassValid: the MCPServer controller's tools/list
	// probe + AgentClass binding-coverage Check will trip with
	// Valid=False reason=AllowlistDrift if either declared tool is
	// missing on the live MCP server (see T13's comment chain).
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
		return map[string]any{
			"results": []map[string]any{
				{"email": "alice@acme.com"},
				{"email": "bob@acme.com"},
			},
		}
	})

	// Block until the AgentClass converges. Without this, the first
	// SendUserMessage races the controller chain — a synthetic inbound
	// can land before the AgentSession controller is ready to handle it.
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	// Block until SpiceDBBootstrap finishes too — AgentClass Valid does
	// NOT imply the bootstrap wrote its tuples (independent reconcile
	// loops + a 5s guardian debounce). crm_owner:owner-1#user must be in
	// SpiceDB before turn 2, or the approver lookup on
	// crm_company:acme-id#owner resolves to nobody and the prompt is
	// routed to an empty set.
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	// And until the guardian has composed this class's grant pairs into the
	// agentsession definition. Approving writes
	// agentsession:<sess>#grant_contact_access_crm_company; that relation lands
	// on a guardian reconcile one debounce AFTER the AgentClass reports Valid,
	// and an approval inside that window is rejected by SpiceDB
	// (FailedPrecondition), stranding the dispatch until it times out.
	h.WaitForGrantSchema(30 * time.Second)

	// LLM script. Five rules total:
	//
	//   1. Turn-1 user message → tool_use list_companies.
	//   2. list_companies tool_result → respond_to_user (turn-1 reply).
	//   3. Turn-2 user message → tool_use list_contacts_for_company.
	//   4. list_contacts_for_company tool_result → respond_to_user (turn-2 reply).
	//   5. respond_to_user's tool_result → end_turn (REPEATING).
	//
	// The repeating end_turn rule fires after BOTH respond_to_user
	// calls: the runner asks the LLM "what next?" once respond_to_user
	// returns its delivered tool_result, and we need to terminate each
	// turn cleanly. Without it the second turn would either re-trigger
	// rule #1 (matching old user text) or fail with "no rule matched"
	// from the runner goroutine while ExpectAgentReply still polls.
	// MCP-synthesized tool calls require the operation_id + _reason audit
	// fields (mcp_tool.go's tcArgs shape). With no SessionContext.Operations
	// wired (the e2e factory leaves it nil), the operation_id is NOT
	// re-validated against the operations registry — any non-empty value
	// passes. The real arguments go inside the `args` sub-object.
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
	// NOT AnyResult: the approved call must actually return contacts. A
	// post-approval authorization denial also yields a tool_result — an
	// error one — which AnyResult would match, letting the script reply
	// with this canned success text and pass a scenario in which the
	// approved call was denied. Requiring the payload makes that a
	// no-rule-matched Fatalf instead.
	h.LLM.OnToolResult("centerdot_list_contacts_for_company", e2e.ResultContains("alice@acme.com")).
		Reply(e2e.RespondToUser("Acme contacts: alice@acme.com, bob@acme.com"))

	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn()).Repeating()

	// Turn 1: ask for companies. No approval expected — list_companies is
	// passthrough. Its writesRelationships side-effect seeds, per company,
	// owner_ref (so the turn-2 ApproverSubject lookup can chain
	// owner_ref->user) and any_user@user:* (so the turn-2 post-approval
	// grant walk has a leaf to resolve).
	h.SendUserMessage("companies created in the last 2 weeks",
		e2e.AsUser("user@example.com"))
	h.ExpectAgentReply(e2e.Contains("Acme", "Beta", "Centerdot"))

	// NO co-owner seeding: this is the PRODUCTION shape. The owner resolver
	// made the requester (user@example.com) the sole session owner; owner-1
	// owns crm_company:acme-id only via the turn-1 owner_ref JIT-write. The
	// eligible approver pool is the RESOURCE's owner-set — owner-1 needs no
	// session standing to approve (see the rationale on
	// authz.ResolveApprovers; the pre-fix intersection model made this exact
	// scenario unapprovable in production, hidden by a SeedSessionCoOwner
	// fixture hack that no production path mirrored).

	// Turn 2: ask for contacts. routeViaSessionGrant bypasses the direct
	// crm_company:acme-id#contact_access check entirely and asks
	// agentsession:<sess>#check_contact_access_crm_company instead; with
	// no grant tuple yet that denies, and enforceMode:always routes the
	// deny to approval. The runner emits tool_approval_request, the
	// pipeline forwards it to the fake permission_request sender, and
	// ExpectApprovalPrompt below matches it.
	h.SendUserMessage("contacts on Acme", e2e.AsUser("user@example.com"))

	// The prompt's Tool field is the LLM-facing prefixed name
	// (centerdot_list_contacts_for_company) — that's what the runner
	// passes through to channelevents.ToolApprovalRequestPayload.Tool.
	// Matching the bare upstream name "list_contacts_for_company" would
	// silently never match.
	approval := h.ExpectApprovalPrompt(
		e2e.ForTool("centerdot_list_contacts_for_company"),
		e2e.ForResource("crm_company:acme-id"),
	)

	// Delivery routing: the request's ApproverSubject must be the gated
	// resource's owner-set — that's what the channel kind fans the prompt
	// out over (owner-1 gets the DM), NOT the session approve-set.
	if got := approval.Prompt().Approver; got != "crm_company:acme-id#owner" {
		t.Fatalf("approval routed to %q; want the resource owner-set crm_company:acme-id#owner", got)
	}

	// Approve as owner-1 — the user the JIT-written owner_ref chain
	// resolves to for crm_company:acme-id#owner. The pipeline's
	// CheckApproverAuthorized validates the clicker owns the resource,
	// writes the per-(session,tool,args) grant tuple, and publishes
	// tool_approval_applied for the runner to resume on.
	approval.Approve(e2e.AsUser("owner-1@example.com"))

	// Final assertion: the resumed dispatch returned contacts and the
	// LLM emitted respond_to_user with the contact emails.
	h.ExpectAgentReply(e2e.Contains("alice@acme.com"))

	// Every non-repeating rule must have fired exactly once — a missed
	// rule means the runner took an unexpected branch (e.g. extra
	// retry, wrong tool dispatch) and the test is reporting a false
	// pass on whatever it did emit.
	h.LLM.AssertAllRulesConsumed()
}
