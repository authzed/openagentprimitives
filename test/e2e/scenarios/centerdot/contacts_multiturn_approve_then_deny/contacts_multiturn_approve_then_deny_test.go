//go:build e2e

// Package contacts_multiturn_approve_then_deny_test drives the least-covered
// approval composition: two approval-gated turns in ONE session where the
// decision KIND flips between turns (approve, then deny) on DIFFERENT
// resources. The single canonical scenario only exercises a homogeneous
// approve; this proves grant isolation across turns — a grant written for one
// resource does NOT satisfy a later call on another.
//
// Three turns:
//
//	Turn 1 — "companies …": list_companies is passthrough (ungated) and
//	  JIT-writes owner_ref + any_user@user:* for every returned company. All
//	  three companies are owned by owner-1 here (the fixture allows any
//	  ownerId), so owner-1 is the eligible approver for BOTH acme-id and
//	  beta-id in the turns below.
//
//	Turn 2 — "contacts on Acme": the check routes via the session grant, of
//	  which none exists yet, so it denies → approval prompt.
//	  While the dispatch is parked, the session surfaces an observable
//	  ToolApprovalPending condition (asserted below). owner-1 APPROVES → a
//	  per-(session,tool,args) grant tuple keyed to crm_company:acme-id is
//	  written → the tool runs → Acme contacts reply.
//
//	Turn 3 — "contacts on Beta": the session-grant check denies again. The
//	  crux: the turn-2 Acme grant does NOT carry over — grants
//	  bind to the resource/args, and beta-id is a different subject — so a fresh
//	  approval fires. owner-1 DENIES → NO grant tuple for beta-id is written and
//	  the MCP tool is never invoked.
//
// Final grant-isolation assertion (the point of the scenario): after the deny,
// SpiceDB carries a grant tuple whose subject is crm_company:acme-id and NONE
// whose subject is crm_company:beta-id.
//
// No real names — Acme / Beta / Centerdot / owner-1 / user@example.com are all
// fictional per AGENTS.md.
package contacts_multiturn_approve_then_deny_test

import (
	"context"
	"regexp"
	"testing"
	"time"

	spicedbv1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/test/e2e"
)

func TestCenterdot_ApproveThenDeny_GrantIsolation(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: "../../../testdata/agent-centerdot-companies",
		// Three turns with two approval round-trips: the 2nd/3rd-turn cold
		// runner respawn regularly exceeds the 10s default under suite load, so
		// use the 30s multi-turn budget the other centerdot/passthrough
		// multi-turn scenarios use.
		DefaultTimeout: 60 * time.Second,
	})

	// Both declared tools need MCP handlers for AgentClass Valid=True (the
	// MCPServer controller's tools/list probe gates on it). Every company is
	// owned by owner-1 so owner-1 is the eligible approver for acme AND beta.
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{
			"results": []map[string]any{
				{"id": "acme-id", "name": "Acme", "ownerId": "owner-1"},
				{"id": "beta-id", "name": "Beta", "ownerId": "owner-1"},
				{"id": "centerdot-id", "name": "Centerdot", "ownerId": "owner-1"},
			},
		}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []map[string]any{{"email": "alice@acme.com"}}}
	})

	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	// And until the guardian has composed this class's grant pairs into the
	// agentsession definition — the approve turn writes a grant relation that
	// lands one debounce after the AgentClass reports Valid. See
	// WaitForGrantSchema.
	h.WaitForGrantSchema(30 * time.Second)

	// LLM script — six rules. Turns 2 and 3 dispatch the same tool against
	// different companyId args; their reply rules are told apart by the
	// tool_result payload, not by arrival order (see the pair below).
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
	h.LLM.OnUserMessage("contacts on Beta").
		Reply(e2e.ToolUse("centerdot_list_contacts_for_company", map[string]any{
			"operation_id": "op-t3",
			"_reason":      "user asked for contacts on Beta",
			"args":         map[string]any{"companyId": "beta-id"},
		}))
	// The first list_contacts_for_company result (turn 2, approved) must carry
	// real contacts; the second (turn 3, denied) must be a
	// SYSTEM_DECISION_DENIED tool_result. Matching on the PAYLOAD rather than
	// on arrival order is what makes the pair load-bearing: with AnyResult a
	// turn-2 post-approval denial matched the first rule and the scenario
	// still replied "Acme contacts: …", passing a run in which the approved
	// call was denied. Now that outcome is a no-rule-matched Fatalf.
	h.LLM.OnToolResult("centerdot_list_contacts_for_company", e2e.ResultContains("alice@acme.com")).
		Reply(e2e.RespondToUser("Acme contacts: alice@acme.com"))
	h.LLM.OnToolResult("centerdot_list_contacts_for_company", e2e.ResultContains("SYSTEM_DECISION_DENIED")).
		Reply(e2e.RespondToUser("The owner denied access to those contacts."))

	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn()).Repeating()

	// Turn 1: companies. No approval — seeds owner_ref for acme + beta.
	h.SendUserMessage("companies created in the last 2 weeks", e2e.AsUser("user@example.com"))
	h.ExpectAgentReply(e2e.Contains("Acme", "Beta", "Centerdot"))

	// NO co-owner seeding (see contacts_owner_approval): owner-1 has
	// standing on both turns purely as acme's and beta's owner via the
	// turn-1 owner_ref JIT-writes — the production shape.

	// Turn 2: contacts on Acme → approval → APPROVE.
	h.SendUserMessage("contacts on Acme", e2e.AsUser("user@example.com"))
	acme := h.ExpectApprovalPrompt(
		e2e.ForTool("centerdot_list_contacts_for_company"),
		e2e.ForResource("crm_company:acme-id"),
	)

	// L1 (observable status): with the dispatch parked on the decision, the
	// session carries ToolApprovalPending=True — the harness's status-level
	// witness that the mid-turn approval is live before any Approve/Deny. (The
	// AwaitingDecision *phase string* is projected by the operator's fold only
	// for a real runner Pod; the in-process factory never reaches that
	// projection, so the pending-approval condition is the observable proxy
	// here. See the audit report.)
	requireEventualCondition(t, h, spiceboxv1alpha1.AgentSessionConditionToolApprovalPending, metav1.ConditionTrue)

	acme.Approve(e2e.AsUser("owner-1@example.com"))
	h.ExpectAgentReply(e2e.Contains("alice@acme.com"))

	// Turn 3: contacts on Beta → approval (the Acme grant does NOT carry over)
	// → DENY.
	h.SendUserMessage("contacts on Beta", e2e.AsUser("user@example.com"))
	beta := h.ExpectApprovalPrompt(
		e2e.ForTool("centerdot_list_contacts_for_company"),
		e2e.ForResource("crm_company:beta-id"),
	)
	beta.Deny(e2e.AsUser("owner-1@example.com"))
	h.ExpectAgentReply(e2e.Matches(regexp.MustCompile(`(?i)denied`)))

	h.LLM.AssertAllRulesConsumed()

	// Grant isolation: the approve wrote a grant tuple keyed to acme-id; the
	// deny wrote none for beta-id. Grants are per-resource/args, so the acme
	// grant never satisfied the beta call.
	granted := grantedCompanyIDs(t, h)
	assert.Contains(t, granted, "acme-id",
		"turn-2 approve must have written a contact_access grant for crm_company:acme-id")
	assert.NotContains(t, granted, "beta-id",
		"turn-3 deny must NOT write a grant for crm_company:beta-id (grants do not carry over)")

	// Every turn boundary in this scenario is a chance to strand the next
	// message: ExpectAgentReply returns mid-turn, so the follow-up is sent
	// while the runner is finishing and idling. This is the flake that used to
	// present as a missing approval prompt.
	h.AssertNoStrandedInbox()
}

// requireEventualCondition polls the single AgentSession until the named status
// condition holds `want`, failing if it never converges. The condition is
// eventually consistent — the pipeline patches it on the reconcile the parked
// approval triggers.
func requireEventualCondition(t *testing.T, h *e2e.Harness, condType string, want metav1.ConditionStatus) {
	t.Helper()
	last := metav1.ConditionStatus("<absent>")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c := meta.FindStatusCondition(singleSession(t, h).Status.Conditions, condType)
		if c != nil {
			last = c.Status
			if c.Status == want {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("AgentSession condition %s never reached %q (last observed %q)", condType, want, last)
}

// grantSubjectIDs returns the subject object IDs of the contact_access grant
// tuples on the single session's agentsession resource. The grant relation is
// grant_<permission>_<resourceType>; a written grant's subject is
// crm_company:<companyId>, so the subject ID is the companyId the grant covers.
// grantedCompanyIDs returns the crm_company ids this session holds a
// contact_access slot grant on.
//
// Reads the RESOURCE side, which is the direction the grant now points: the
// tuple is crm_company:<id>#slot_grant_contact_access:<s>. It used
// to be the mirror image, and that reversal is why the schema no longer needs a
// wildcard leaf for the approval walk to pass.
func grantedCompanyIDs(t *testing.T, h *e2e.Harness) []string {
	t.Helper()
	stream, err := h.SpiceDB.ReadRelationships(context.Background(),
		&spicedbv1.ReadRelationshipsRequest{
			RelationshipFilter: &spicedbv1.RelationshipFilter{
				ResourceType:     "crm_company",
				OptionalRelation: authz.SlotGrantRelationName("contact_access"),
			},
		})
	require.NoError(t, err, "ReadRelationships crm_company slot grants")
	var ids []string
	for {
		resp, recvErr := stream.Recv()
		if recvErr != nil {
			break
		}
		ids = append(ids, resp.Relationship.Resource.ObjectId)
	}
	return ids
}

// singleSession returns the one AgentSession CR in the harness namespace.
func singleSession(t *testing.T, h *e2e.Harness) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(context.Background(), &sessions), "list AgentSessions")
	require.Len(t, sessions.Items, 1, "expected exactly one AgentSession")
	return &sessions.Items[0]
}
