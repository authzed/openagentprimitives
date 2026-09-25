//go:build e2e

// Package contacts_owner_deny_test mirrors contacts_owner_approval's
// two-turn flow but the owner DENIES the second-turn approval. The
// scenario validates that the deny path is symmetric with approve at
// the pipeline level — same envelopes, same NATS subjects, same runner
// resume — and that the runner surfaces the denial to the LLM as a
// SYSTEM_DECISION_DENIED tool_result rather than ever executing the
// underlying MCP tool.
//
// Turn 1 mirrors the approval scenario exactly: list_companies is
// passthrough (ungated) and JIT-writes owner_ref tuples so the turn-2
// ApproverSubject lookup can resolve owner-1@example.com as the approver
// for crm_company:acme-id.
//
// Turn 2 — "contacts on Acme":
//   - Runner check on crm_company:acme-id#contact_access routes via the
//     session grant (same as the approval scenario) and denies: no grant
//     tuple has been written for this session/resource/args yet.
//   - Runner publishes tool_approval_request; pipeline forwards to
//     the fake permission_request sender; ExpectApprovalPrompt matches.
//   - Approval.Deny(AsUser("owner-1@example.com")) publishes a
//     tool_approval_decision with decision="deny". The pipeline still
//     validates clicker ∈ ApproverSubject (denial by a non-approver
//     would be rejected, same as approval), then publishes
//     tool_approval_applied with Decision="deny" — NO SpiceDB grant
//     tuple is written.
//   - Runner resumes the paused dispatch via Orchestrator.Await, sees
//     d.Approved=false, builds a tool_result with content prefixed
//     "SYSTEM_DECISION_DENIED:" via approval_outcome.go's
//     approvalFailureContent. The underlying MCP list_contacts_for_company
//     is NEVER invoked (denial short-circuits dispatch).
//   - LLM receives the denial-shaped tool_result, third rule matches
//     on the SYSTEM_DECISION_DENIED prefix, emits respond_to_user with a
//     denial-explanation text.
//   - ExpectAgentReply matches against /denied/i — the runner's
//     SYSTEM_DECISION_DENIED message contains "DENIED" but the LLM's
//     own respond_to_user output is what reaches the channel.
//
// Why we still register the list_contacts_for_company MCP handler:
// the MCPServer controller's tools/list probe + AgentClass binding
// coverage gate Valid=True on the declared tool being present on the
// upstream server. Without the handler, WaitForAgentClassValid would
// time out before turn 1 ever fires. The handler exists as schema
// scaffolding; denial guarantees it's never invoked — and the handler
// now returns contactPayloadSentinel instead of an empty result set, so
// if that guarantee ever regresses the sentinel is what makes it loud:
// the tightened OnToolResult rule stops matching (ScriptedLLM.Send fails
// with a request dump), every request the LLM observed is scanned for the
// sentinel (assertContactSentinelNeverObserved), and contactsCallCount is
// asserted to be zero. Before this fix the handler returned an empty
// result set and the rule matched via AnyResult() regardless of content,
// so a fail-open regression that actually ran the tool would have been
// silently absorbed by the same canned "denied" reply text — the exact
// 19-of-20 blind spot this package's AnyResult() doc comment
// (test/e2e/scripted_llm_helpers.go) warns about, mirrored onto the
// gated-tool side.
//
// No real names — Acme / Beta / Centerdot / owner-1 / user@example.com
// are all fictional per AGENTS.md.
package contacts_owner_deny_test

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/e2e"
)

// contactPayloadSentinel is a distinctive, obviously-fake contact identifier
// (per AGENTS.md's "no real names" — this is a marker string, not a person)
// that the gated MCP tool returns instead of an empty result set. An empty
// result set is indistinguishable from "the tool was never called" — exactly
// the blindness this scenario exists to close (see the handler comment
// below): a fail-open regression that actually runs the tool needs a payload
// that is impossible to produce by accident, so its appearance in ANY
// request the ScriptedLLM observed (see assertContactSentinelNeverObserved)
// is unambiguous proof the tool executed.
const contactPayloadSentinel = "CONTACT-PAYLOAD-MUST-NOT-APPEAR"

func TestCenterdot_ContactsDeniedByOwner(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: "../../../testdata/agent-centerdot-companies",
		// Turn 2's approval prompt waits on a COLD runner respawn (turn 1 goes
		// Idle/AgentWorkComplete and the runner is torn down before turn 2's
		// SendUserMessage). That cold path can exceed the 10s default under
		// suite contention — turn 1 is warmed by the preceding 30s readiness
		// waits, so only the second turn flakes. Match the 30s headroom every
		// other multi-turn e2e scenario already uses.
		DefaultTimeout: 30 * time.Second,
	})

	// Both MCP tools must have handlers for AgentClass Valid=True
	// (see package-level comment). list_contacts_for_company won't
	// actually be invoked — denial short-circuits dispatch before the
	// runner reaches Execute on the tool kind.
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
		// Sentinel payload, not an empty result set: an empty result would
		// be indistinguishable from "the tool never ran" and a fail-open
		// regression that actually invokes this handler would go unnoticed.
		// If the deny path ever regresses and this DOES get called, the
		// sentinel below leaks into the tool_result and, via the tightened
		// OnToolResult matcher and the request-scan/call-count assertions
		// after Turn 2, fails the test loudly instead of being masked by
		// the canned "denied" reply text.
		return map[string]any{"results": []map[string]any{
			{"email": contactPayloadSentinel + "@acme.com"},
		}}
	})

	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// LLM script. Five rules, identical shape to the approval scenario
	// except the third rule's reply is denial-framed instead of
	// contact-listing-framed.
	//
	// Args wrap in {operation_id, _reason, args} per mcp_tool.go's tcArgs
	// contract — the e2e InProcessRunnerFactory leaves SessionContext.Operations
	// nil, so operation_id is not re-validated against the registry, but the
	// envelope shape is still enforced.
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

	// On the denied tool_result, the LLM acknowledges the denial. Tightened
	// from AnyResult(): Deny() drives Orchestrator.Await down the
	// deterministic d.Approved=false branch — there is no timeout/publish-
	// error variance on this path (contrast contacts_owner_timeout, whose
	// tool_result content genuinely can vary under contention — see that
	// scenario for why AnyResult() stays there) — so the tool_result content
	// is always prefixed "SYSTEM_DECISION_DENIED:" (approval_outcome.go).
	// Matching on that prefix rather than the full message keeps the test
	// decoupled from prompt-engineering wording while still refusing to
	// match a SUCCESS payload: if the deny path ever regresses open, no
	// rule matches this tool_result and ScriptedLLM.Send fails the test
	// loudly with its full request dump, rather than silently falling
	// through to AnyResult()'s canned reply.
	h.LLM.OnToolResult("centerdot_list_contacts_for_company", e2e.ResultContains("SYSTEM_DECISION_DENIED")).
		Reply(e2e.RespondToUser("The owner denied access to those contacts."))

	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn()).Repeating()

	// Turn 1: companies (no approval; list_companies is passthrough).
	h.SendUserMessage("companies created in the last 2 weeks",
		e2e.AsUser("user@example.com"))
	h.ExpectAgentReply(e2e.Contains("Acme", "Beta", "Centerdot"))

	// NO co-owner seeding (see contacts_owner_approval): owner-1 has
	// standing to Deny purely as crm_company:acme-id's owner via the turn-1
	// owner_ref JIT-write — the production shape.

	// Turn 2: contacts → approval prompt → DENY.
	h.SendUserMessage("contacts on Acme", e2e.AsUser("user@example.com"))

	approval := h.ExpectApprovalPrompt(
		e2e.ForTool("centerdot_list_contacts_for_company"),
		e2e.ForResource("crm_company:acme-id"),
	)

	// Deny as owner-1 — same approver-validity check as approve. A
	// non-approver Deny would be rejected by the pipeline's
	// LookupSubjectIncludes the same way a non-approver Approve would.
	approval.Deny(e2e.AsUser("owner-1@example.com"))

	// Runner resumes with a SYSTEM_DECISION_DENIED tool_result; LLM's
	// respond_to_user output (matched by tool name in rule 3) reaches
	// the channel. The /denied/i regexp keeps the assertion robust to
	// exact-wording changes on either side of the runner→LLM boundary.
	h.ExpectAgentReply(e2e.Matches(regexp.MustCompile(`(?i)denied`)))

	// The mirror of AnyResult()'s blind spot, closed two independent ways:
	//
	// 1. assertContactSentinelNeverObserved scans every request the
	//    ScriptedLLM actually saw for the sentinel. This is NOT the same as
	//    checking the channel-visible reply text: rule 3's Reply(...) above
	//    is a fixed canned string ("The owner denied access...") that never
	//    echoes the tool's real payload, so a reply.Text-only check would be
	//    vacuous — it would read the same whether the tool ran or not. The
	//    tool_result CONTENT the LLM was fed, by contrast, only contains the
	//    sentinel if the MCP tool actually executed and returned it.
	// 2. contactsCallCount is the most direct proof available: the gated MCP
	//    tool itself was never invoked. A deny short-circuits dispatch before
	//    Execute (see the package doc comment); this checks that fact
	//    straight off the stub's call log rather than inferring it from
	//    LLM-script matching at all.
	assertContactSentinelNeverObserved(t, h)
	assert.Equal(t, 0, contactsCallCount(t, h),
		"list_contacts_for_company must never execute — a deny short-circuits dispatch before Execute")

	h.LLM.AssertAllRulesConsumed()
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
// assertNoLeak / test/e2e's established canary-scan idiom: the deny path's
// only load-bearing witness that the sentinel never reached the model is the
// tool_result content of a LATER request, not the final reply (whose text is
// a fixed canned string independent of the tool's actual output).
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
