//go:build e2e

// Package contacts_requester_click_rejected_test pins the click-time HALF of
// the resource-owner approver model (see contacts_owner_approval for the
// raise-time half and the full fixture walkthrough):
//
//   - the REQUESTER — the session's sole owner — clicks Approve on their own
//     agent's gated call, and channelsd MUST reject the click (spectator
//     path: pending entry stays, no applied envelope, no grant tuple),
//     because session standing does not confer approval standing on someone
//     else's resource (no self-approval);
//   - the RESOURCE OWNER (owner-1, standing purely via the turn-1 owner_ref
//     JIT-write — zero session standing) then clicks Approve and the flow
//     completes normally.
//
// Together with contacts_owner_approval this pins both directions of
// authz.CheckApproverAuthorized's resource branch: ownership authorizes
// without session standing, and session standing does not substitute for
// ownership.
//
// The AnyResult()-blindness mirror (see test/e2e/scripted_llm_helpers.go's
// AnyResult doc comment) shows up differently here than in
// contacts_owner_deny / contacts_owner_timeout: those scenarios expect the
// gated call to fail, so tightening their OnToolResult rule onto the deny-
// shaped tool_result's distinguishing prefix directly rejects a fail-open
// success. Here the gated call is SUPPOSED to succeed — via the resource
// owner's legitimate Approve — so there is no error-shaped substring to pin
// the rule to, and tightening AnyResult() to match the (single, fixed)
// success payload wouldn't discriminate a regression: an illegitimate grant
// from the requester's rejected self-click would produce the exact same
// payload as the legitimate one. What actually proves self-approval didn't
// smuggle the gated call through is WHEN the MCP tool ran, not what it
// returned — see contactsCallCount below, asserted ==0 right after the
// rejected click (before the owner ever approves) and ==1 only after the
// legitimate approval.
package contacts_requester_click_rejected_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/test/e2e"
)

// contactPayloadSentinel is a distinctive, obviously-fake contact identifier
// (no real names, per AGENTS.md) in place of a plausible-looking address —
// purely for cross-scenario grep-ability with contacts_owner_deny /
// contacts_owner_timeout's sentinels of the same shape. Unlike those two,
// this scenario's happy path legitimately produces this payload (the owner's
// approval is supposed to succeed); it is not a "must never appear" marker
// here, just a recognizable one.
const contactPayloadSentinel = "CONTACT-PAYLOAD-OWNER-APPROVED-ONLY"

func TestCenterdot_RequesterClickRejected_OwnerApproves(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: "../../../testdata/agent-centerdot-companies",
		// Turn 2's approval prompt waits on a COLD runner respawn; match the
		// 30s headroom the other multi-turn centerdot scenarios use.
		DefaultTimeout: 90 * time.Second,
	})

	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{
			"results": []map[string]any{
				{"id": "acme-id", "name": "Acme", "ownerId": "owner-1"},
				{"id": "beta-id", "name": "Beta", "ownerId": "owner-2"},
			},
		}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{
			"results": []map[string]any{
				{"email": contactPayloadSentinel + "@acme.com"},
			},
		}
	})

	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	// And until the guardian has composed this class's grant pairs into the
	// agentsession definition — the owner's approve writes a grant relation
	// that lands one debounce after the AgentClass reports Valid. See
	// WaitForGrantSchema.
	h.WaitForGrantSchema(30 * time.Second)

	h.LLM.OnUserMessage("companies created in the last 2 weeks").
		Reply(e2e.ToolUse("centerdot_list_companies", map[string]any{
			"operation_id": "op-t1",
			"_reason":      "user asked for recent companies",
			"args":         map[string]any{"sinceDays": 14},
		}))
	h.LLM.OnToolResult("centerdot_list_companies", e2e.AnyResult()).
		Reply(e2e.RespondToUser("Found 2 companies: Acme, Beta"))

	h.LLM.OnUserMessage("contacts on Acme").
		Reply(e2e.ToolUse("centerdot_list_contacts_for_company", map[string]any{
			"operation_id": "op-t2",
			"_reason":      "user asked for contacts on Acme",
			"args":         map[string]any{"companyId": "acme-id"},
		}))
	// AnyResult() is intentionally left untightened here — see the
	// package-level comment for why an error-shaped substring doesn't apply
	// to this scenario's (legitimately successful) gated call, and why the
	// call-count-timing assertions below are the actual proof.
	h.LLM.OnToolResult("centerdot_list_contacts_for_company", e2e.AnyResult()).
		Reply(e2e.RespondToUser("Acme contacts: " + contactPayloadSentinel + "@acme.com"))

	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn()).Repeating()

	// Turn 1: companies — JIT-writes crm_company:acme-id#owner_ref@crm_owner:owner-1.
	h.SendUserMessage("companies created in the last 2 weeks",
		e2e.AsUser("user@example.com"))
	h.ExpectAgentReply(e2e.Contains("Acme", "Beta"))

	// Turn 2: contacts → the Check denies (requester lacks contact_access on
	// acme-id) and enforceMode:always raises the approval.
	h.SendUserMessage("contacts on Acme", e2e.AsUser("user@example.com"))

	approval := h.ExpectApprovalPrompt(
		e2e.ForTool("centerdot_list_contacts_for_company"),
		e2e.ForResource("crm_company:acme-id"),
	)

	// The REQUESTER clicks Approve on their own request. channelsd's
	// CheckApproverAuthorized must reject it: user@example.com owns the
	// session but not crm_company:acme-id, and only resource ownership
	// grants approval standing. The rejected click takes the spectator
	// path — the pending entry stays and NO tool_approval_applied is
	// published, which the short WaitApplied window verifies (an error is
	// the EXPECTED outcome here; success means self-approval regressed).
	approval.Approve(e2e.AsUser("user@example.com"))
	shortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := approval.WaitApplied(shortCtx); err == nil {
		t.Fatal("requester's click produced a tool_approval_applied envelope — self-approval must be rejected")
	}

	// WaitApplied's short context blocked for the full 2s (no envelope ever
	// arrived), which is ample time for any async side effect of a regressed
	// self-approval to have already fired — so this call-count check is not
	// racing the decision path. The direct, content-independent proof that
	// the rejected click never smuggled the gated call through: it must not
	// have run yet at all.
	assert.Equal(t, 0, contactsCallCount(t, h),
		"list_contacts_for_company must NOT execute merely because the requester clicked Approve on their own request — self-approval must never reach dispatch")

	// The resource owner clicks Approve — authorized purely via the
	// owner_ref chain, with zero session standing — and the paused dispatch
	// resumes.
	approval.Approve(e2e.AsUser("owner-1@example.com"))
	h.ExpectAgentReply(e2e.Contains(contactPayloadSentinel))

	// Exactly one invocation — from the resource owner's legitimate
	// approval. A count of 0 here would mean the reply above matched
	// something other than the gated tool's real payload; a count > 1 would
	// mean the rejected self-click eventually smuggled through an extra
	// (phantom) call in addition to the legitimate one. Either would mean
	// the earlier ==0 check above raced a regression rather than disproving
	// it.
	assert.Equal(t, 1, contactsCallCount(t, h),
		"list_contacts_for_company must execute exactly once, via the resource owner's approval")

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
