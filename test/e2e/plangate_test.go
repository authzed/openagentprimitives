//go:build e2e

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// planGateLoggingOverride re-applies the centerdot fixture's AgentClass with
// the plan gate at logging.
//
// The whole spec is restated, not just spec.authz: ExtraManifests REPLACES the
// object rather than patching it, so a partial spec drops systemPrompt and the
// class fails Valid with "exactly one of inline or configMapRef must be set".
// Everything below except the trailing authz block is verbatim from
// testdata/agent-centerdot-companies/03-agent.yaml, so the two arms of the
// neutrality test differ only in the gate's mode.
const planGateLoggingOverride = `
apiVersion: agentprimitives.authzed.com/v1alpha1
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
      DO NOT deploy to a real cluster.

      You operate on companies and contacts. Available tools:
        - centerdot_list_companies(sinceDays): list companies created recently
        - centerdot_list_contacts_for_company(companyId): list contacts
  agentIdentity: centerdot-identity
  mcpServers:
    - name: centerdot
      ref: centerdot-companies
  budget:
    maxTurns: 10
    maxTokens: 50000
    maxDuration: 5m
  authz:
    slots:
      - resourceType: crm_company
        description: "A CRM company record"
        permission: contact_access
    planGate:
      mode: logging
      # Observe-only: the gate runs and records, but does not require a plan.
      #
      # requirePlan now DERIVES from the mode, so leaving this unset would make
      # these tests assert ceiling behaviour in a session whose very first call
      # is refused for having no plan — a different fact entirely. False is the
      # rollout stage these tests are written about: run the gate, change
      # nothing the agent may do, and see what it records.
      requirePlan: false
`

// planGateRecords reads the session's plan-gate log.
//
// WithSystemApproval is required: the memory data plane is capability-gated and
// fail-closed, so a bare context is refused. The capability is an explicit grant
// the test makes on its own behalf, never an ambient one.
func planGateRecords(t *testing.T, h *e2e.Harness, ns, name string) []plangateaudit.Content {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "e2e-plangate-test")
	recs, err := plangateaudit.List(ctx, h.MemStore(),
		memory.Scope{Kind: "session", ID: ns + "/" + name})
	require.NoError(t, err, "listing plan_gate_audit records")
	return recs
}

// startPlanGateAgent boots the centerdot fixture and scripts one turn that
// calls centerdot_list_companies.
//
// Note the two names: h.MCP.OnTool registers the SERVER-side name
// ("list_companies"), while the LLM calls the prefixed LLM-facing name
// ("centerdot_list_companies"). Getting that backwards fails with "unknown
// tool", not with a gate error.
//
// The tool matters: the gate sits at PreToolCall, and trivial-permission meta
// tools (respond_to_user, agent_work_complete) bypass that pipeline entirely
// (loop.go's gatePipeline guard). They carry no permission handle so there is
// nothing for the gate to govern — correct, but it means a scenario driven only
// by meta tools would exercise nothing and pass for the wrong reason.
// The tool is stateImpact: readonly with a real Check, so it traverses
// the gate for real.
// planGateHarness boots the fixture and its MCP tools WITHOUT scripting the
// LLM, so a test can drive its own turn shape.
func planGateHarness(t *testing.T, extra []string) *e2e.Harness {
	t.Helper()
	h := e2e.Start(t, e2e.Options{
		AgentDir:       "testdata/agent-centerdot-companies",
		ExtraManifests: extra,
	})

	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	return h
}

func startPlanGateAgent(t *testing.T, extra []string) *e2e.Harness {
	t.Helper()
	h := planGateHarness(t, extra)

	// Rule ORDER matters (see chat_resume_test.go): the tool_result rules go
	// FIRST, so that once a result lands the turn advances. A Repeating
	// OnUserMessage rule otherwise keeps re-matching the latest human text —
	// matchUserText walks back past tool results — and the agent re-emits the
	// same tool_use until MaxTurnsExceeded.
	//
	// AnyResult matches the ERROR result too, which is deliberate. The gate
	// runs at order 19, BEFORE ToolCallAuthz at order 20, so it observes and
	// records the call whether or not the downstream Check later denies it.
	// That keeps this test measuring the gate rather than the fixture's
	// SpiceDB seeding.
	h.LLM.OnToolResult("centerdot_list_companies", e2e.AnyResult()).
		Reply(e2e.RespondToUser("no companies found")).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()
	h.LLM.OnUserMessage("list the companies").
		Reply(e2e.ToolUse("centerdot_list_companies", map[string]any{})).Repeating()
	h.LLM.On(func(llm.Request) bool { return true }).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()

	return h
}

// Slice 1's contract, stated as a test: with the gate at logging, a session
// behaves EXACTLY as it does with the gate disabled. Both arms run the same
// fixture, the same scripted LLM and the same turn; the only difference is the
// mode, so any divergence is the gate changing behavior.
func TestE2E_planGateLoggingIsBehaviorNeutral(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
	}{
		{name: "disabled: agent calls the tool and replies", extra: nil},
		{name: "logging: agent calls the tool and replies identically",
			extra: []string{planGateLoggingOverride}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := startPlanGateAgent(t, tc.extra)

			h.SendUserMessage("list the companies")
			got := h.ExpectAgentReply(e2e.Contains("no companies found"))

			assert.Contains(t, got.Text, "no companies found",
				"the gate must not change what the agent produces")
		})
	}
}

// The dataset is the point of the slice: logging must actually WRITE records
// for a permissioned call, or slices 2-6 have nothing to be driven by.
func TestE2E_planGateLoggingRecordsAPermissionedCall(t *testing.T) {
	h := startPlanGateAgent(t, []string{planGateLoggingOverride})

	h.SendUserMessage("list the companies")
	h.ExpectAgentReply(e2e.Contains("no companies found"))

	ns, name := e2e.SessionRefForTest(h)
	recs := planGateRecords(t, h, ns, name)
	require.NotEmpty(t, recs, "logging must record the permissioned calls it observed")

	var sawGatedTool bool
	for _, r := range recs {
		assert.Equal(t, "logging", r.Mode, "every record must name the mode that produced it")
		require.NotNil(t, r.PhaseIndex, "every per-call record must name its phase")
		assert.Equal(t, int32(0), *r.PhaseIndex, "the seed plan has exactly one phase")
		assert.NotEmpty(t, r.PlanDigest, "and must pin the plan it was gated against")
		if r.Tool == "centerdot_list_companies" {
			sawGatedTool = true
			assert.NotEmpty(t, r.Handle, "a permissioned tool must resolve to a handle")
		}
	}
	assert.True(t, sawGatedTool,
		"centerdot_list_companies is stateImpact: readonly and must traverse the gate; "+
			"if this fails the gate is not reached on the production path")
}

// The gate is off by default: a session that never opted in writes nothing.
func TestE2E_planGateDisabledRecordsNothing(t *testing.T) {
	h := startPlanGateAgent(t, nil)

	h.SendUserMessage("list the companies")
	h.ExpectAgentReply(e2e.Contains("no companies found"))

	ns, name := e2e.SessionRefForTest(h)
	assert.Empty(t, planGateRecords(t, h, ns, name), "a disabled gate must not write records")
}

// The synthesized ceiling holds the whole surface, so nothing the agent
// actually calls can fall outside it. A would_deny here means the ceiling and
// the dispatcher disagree — the one failure a whole-surface ceiling must not have.
func TestE2E_planGateLoggingNeverWouldDenyARealCall(t *testing.T) {
	h := startPlanGateAgent(t, []string{planGateLoggingOverride})

	h.SendUserMessage("list the companies")
	h.ExpectAgentReply(e2e.Contains("no companies found"))

	ns, name := e2e.SessionRefForTest(h)
	for _, r := range planGateRecords(t, h, ns, name) {
		// would_deny SPECIFICALLY, not "anything that is not allow". The claim
		// here is that the ceiling and the dispatcher agree — a would_deny is
		// the one outcome that means they do not. Asserting "every record is
		// allow" also catches denials that have nothing to do with the ceiling
		// (requirePlan being the first), and reports them as a ceiling bug.
		assert.NotEqual(t, plangateaudit.OutcomeWouldDeny, r.Outcome,
			"tool %q resolved to handle %q and fell outside the session ceiling; "+
				"the synthesized plan must contain the entire surface", r.Tool, r.Handle)
	}
}

// twoPhasePlanArgs declares a read-only recon phase and a write phase. The
// centerdot fixture's list_companies is stateImpact: readonly, so it belongs to
// phase 0; list_contacts_for_company is the phase-1 work.
const twoPhasePlanArgs = `{
  "name":"main",
  "items":[{"id":"s1","label":"list companies","status":"pending","phase":"recon"}],
  "phases":[
    {"id":"recon","label":"List the companies","why":"I need the company list before I can pick one",
     "permissions":[{"handle":"perm:list:crm_company","why":"to list companies"}]},
    {"id":"detail","label":"Fetch contacts","why":"the user asked who works there",
     "requires":[{"phase":"recon","why":"I need a company id first"}],
     "permissions":[{"handle":"perm:contact_access:crm_company","why":"to read that company's contacts"}]}
  ]
}`

// THE agent-authored-phase end-to-end test.
//
// Every piece was unit-green, but until this test the whole chain had
// never executed together: schema validation → freeze → the append-only write →
// the fold → ceiling resolution → the gate → select_phase → re-fold. That gap
// is not hypothetical — the fork derivation was fully unit-green and silently
// broken through the real path, and only a path-level test caught it.
func TestE2E_planGateMultiPhaseChainRunsEndToEnd(t *testing.T) {
	h := planGateHarness(t, []string{planGateLoggingOverride})

	// Rule order matters: tool_result rules first, so the turn advances once a
	// result lands (see startPlanGateAgent's note).
	h.LLM.OnToolResult("update_plan", e2e.AnyResult()).
		Reply(e2e.ToolUse("centerdot_list_companies", map[string]any{})).Repeating()
	h.LLM.OnToolResult("centerdot_list_companies", e2e.AnyResult()).
		Reply(e2e.RespondToUser("no companies found")).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()
	h.LLM.OnUserMessage("list the companies").
		Reply(e2e.ToolUse("update_plan", mustArgs(t, twoPhasePlanArgs))).Repeating()

	h.SendUserMessage("list the companies")
	h.ExpectAgentReply(e2e.Contains("no companies found"))

	ns, name := e2e.SessionRefForTest(h)
	recs := planGateRecords(t, h, ns, name)
	require.NotEmpty(t, recs)

	// The declared phases must have been FROZEN and recorded — one record per
	// phase, each carrying its ceiling and the fields a rebuild needs.
	var approvals []plangateaudit.Content
	for _, r := range recs {
		if r.Event == plangateaudit.EventPlanApproved {
			approvals = append(approvals, r)
		}
	}
	require.Len(t, approvals, 2, "both declared phases must be frozen and recorded")
	for _, a := range approvals {
		assert.NotEmpty(t, a.PlanDigest, "a frozen phase must name its plan")
		assert.NotEmpty(t, a.Ceiling, "and carry the ceiling it was approved for")
		require.NotNil(t, a.PhaseIndex)
	}
	assert.Equal(t, approvals[0].PlanDigest, approvals[1].PlanDigest,
		"both phases belong to ONE frozen plan")

	// The plan must be reconstructable from the log with an identical digest —
	// the invariant the fork path silently violated before it was pinned.
	rebuilt, ok := plangate.PlanFromRecords(recs)
	require.True(t, ok, "the frozen plan must be rebuildable from the log alone")
	assert.Equal(t, approvals[0].PlanDigest, rebuilt.Digest(),
		"a plan rebuilt from the log must BE the recorded plan, or every fold discards it")

	// And the gate must have governed the real call against that plan.
	var gated bool
	for _, r := range recs {
		if r.Tool == "centerdot_list_companies" && r.Handle != "" {
			gated = true
			assert.Equal(t, rebuilt.Digest(), r.PlanDigest,
				"the call must be gated against the frozen plan, not a stale or empty one")
		}
	}
	assert.True(t, gated, "the permissioned call must have reached the gate")
}

func mustArgs(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	return m
}
