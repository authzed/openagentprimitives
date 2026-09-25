package steelthread_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/systemprompt"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// TestDeriveAssertions_ReplyAndAuthz pins the two claims a capture CAN make.
func TestDeriveAssertions_ReplyAndAuthz(t *testing.T) {
	recs := steelthread.Records{Decisions: []authzdecision.Decision{
		{ResourceType: "crm_company", ResourceID: "c1", Permission: "contact_access", Outcome: "denied"},
		{ResourceType: "crm_company", ResourceID: "c1", Permission: "contact_access", Outcome: "allowed"},
	}}
	f := steelthread.Folded{LLM: []bt.LLMStep{
		{Reply: []bt.ReplyPart{{Text: "no widgets found"}}},
	}}

	got := steelthread.DeriveAssertions(recs, f)
	assert.Equal(t, []string{"no widgets found"}, got.AgentReplyContains)
	require.NotNil(t, got.Authz, "Authz must be set when the run recorded decisions")
	assert.Equal(t, "allowed", got.Authz.Decisions["crm_company:c1#contact_access"],
		"the FINAL outcome per key: denied-then-approved-then-allowed resolves to allowed")
}

// TestDeriveAssertions_SystemPromptIsLeftEmpty pins a deliberate omission, not
// an oversight. What matters in a prompt is a judgement; a capture emitting a
// guessed assertion would produce a claim nobody meant and that nobody can
// defend when it later fails.
func TestDeriveAssertions_SystemPromptIsLeftEmpty(t *testing.T) {
	recs := steelthread.Records{SystemPrompts: []systemprompt.Content{{Prompt: "You are a fixture agent."}}}
	assert.Empty(t, steelthread.DeriveAssertions(recs, steelthread.Folded{}).SystemPromptContains)
}

// TestDeriveAssertions_NoDecisionsLeavesAuthzNil is the negative control for
// the authz claim: a run that never checked a permission must not acquire an
// Authz assertion it has nothing to back.
func TestDeriveAssertions_NoDecisionsLeavesAuthzNil(t *testing.T) {
	got := steelthread.DeriveAssertions(steelthread.Records{}, steelthread.Folded{})
	assert.Nil(t, got.Authz)
}

// TestDeriveAssertions_PlanGate pins the two plan-gate facts a capture can
// state without re-deriving the gate's own invariants: whether it wrote
// anything at all, and how many phases froze — both read straight off a
// field the driver's own checkPlanGate compares against (see
// test/e2e/threadrun/driver.go, which counts EventPlanApproved records the
// same way).
func TestDeriveAssertions_PlanGate(t *testing.T) {
	cases := []struct {
		name string
		gate []plangateaudit.Content
		want *bt.PlanGateAssertions
	}{
		{
			name: "no gate records at all: NoRecords",
			gate: nil,
			want: &bt.PlanGateAssertions{NoRecords: true},
		},
		{
			name: "phases froze: FrozenPhases counts EventPlanApproved records",
			gate: []plangateaudit.Content{
				{Event: plangateaudit.EventPlanApproved, PhaseIndex: new(int32(0))},
				{Event: plangateaudit.EventPlanApproved, PhaseIndex: new(int32(1))},
				{Event: plangateaudit.EventGateAllowed, Tool: "list_companies"},
			},
			want: &bt.PlanGateAssertions{FrozenPhases: 2},
		},
		{
			name: "gate active but nothing ever froze: nothing mechanical to claim",
			gate: []plangateaudit.Content{
				{Event: plangateaudit.EventGateDenied, Tool: "list_companies"},
			},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := steelthread.DeriveAssertions(steelthread.Records{Gate: tc.gate}, steelthread.Folded{})
			assert.Equal(t, tc.want, got.PlanGate)
		})
	}
}

// TestGoldenTrace_MatchesTraceLines pins that the capture emits EXACTLY what
// the driver will later compare against. Two renderings of the same logs is
// the standard way this kind of harness rots: the golden diffs on day one.
func TestGoldenTrace_MatchesTraceLines(t *testing.T) {
	recs := steelthread.Records{
		Gate: []plangateaudit.Content{{Event: "phase_frozen", PhaseIndex: new(int32(0)), Handle: "recon"}},
		Decisions: []authzdecision.Decision{
			{ResourceType: "crm_company", ResourceID: "c1", Permission: "list", Outcome: "allowed"},
			{ResourceType: "crm_company", ResourceID: "c1", Permission: "contact_access", Outcome: "denied"},
		},
	}
	want := strings.Join(bt.TraceLines(recs.Gate, recs.Decisions), "\n") + "\n"
	assert.Equal(t, want, string(steelthread.GoldenTrace(recs)))
}

// TestDeriveApprovalSettings splits categories by what was actually DECIDED,
// not by what was asked. A category that was requested and refused belongs in
// autoDeny; putting it in autoApprove would make the replay grant something
// the real run withheld.
//
// approval.Request / approval.Outcome carry no Category (see
// pkg/memory/kinds/approval) — so a tool-call approval's category cannot be
// read off Records.Approvals at all, only whether it was answered and by
// whom. The plan gate's two categories (plan_phase, plan_amendment), by
// contrast, ARE recoverable from Records.Gate: a per-call gate record's
// Handle is empty for a plain phase ask and carries the one permission an
// amendment asked to add otherwise (see pendingPlanGate.handle in
// pkg/agent/runner/host_approval.go).
func TestDeriveApprovalSettings(t *testing.T) {
	recs := steelthread.Records{
		Approvals: map[string]approval.Pair{
			"tu-1": {
				Request: &approval.Request{ToolName: "tracker_update_issue"},
				Outcome: &approval.Outcome{Decision: "approved", Approver: "user:YWxpY2U"},
			},
		},
		Gate: []plangateaudit.Content{
			// plan_phase: requested then approved.
			{Event: plangateaudit.EventCardBuilt, PlanDigest: "d1", PhaseIndex: new(int32(0)), Outcome: plangateaudit.OutcomeDenied, Mode: "enforcing"},
			{Event: plangateaudit.EventPhaseApproved, PlanDigest: "d1", PhaseIndex: new(int32(0)), Mode: "enforcing"},
			// plan_amendment: requested and left denied (never approved).
			{Event: plangateaudit.EventAmendmentRequested, PlanDigest: "d1", PhaseIndex: new(int32(0)), Handle: "perm:write:tracker_issue", Outcome: plangateaudit.OutcomeDenied, Mode: "enforcing"},
		},
	}

	yes, no, as, undetermined := steelthread.DeriveApprovalSettings(recs)
	assert.Equal(t, []string{"plan_phase"}, yes, "deduplicated and sorted, so a re-capture is byte-identical")
	assert.Equal(t, []string{"plan_amendment"}, no)
	assert.Equal(t, "user:YWxpY2U", as)
	assert.Equal(t, []string{"tu-1"}, undetermined,
		"a tool-call approval's category cannot be read from durable records — reported, not guessed")
}

// TestDeriveApprovalSettings_DenialWinsForAMixedCategory covers a category
// that both cleared and was refused across the same run (two different
// phases). The replay driver's auto-approver matches by CATEGORY NAME, not
// by which phase asked, and it checks AutoDeny before AutoApprove — so the
// only way to honor "never grant what the run withheld" is to leave the
// category out of AutoApprove entirely once any instance of it was denied.
func TestDeriveApprovalSettings_DenialWinsForAMixedCategory(t *testing.T) {
	recs := steelthread.Records{Gate: []plangateaudit.Content{
		{Event: plangateaudit.EventCardBuilt, PlanDigest: "d1", PhaseIndex: new(int32(0)), Outcome: plangateaudit.OutcomeDenied, Mode: "enforcing"},
		{Event: plangateaudit.EventPhaseApproved, PlanDigest: "d1", PhaseIndex: new(int32(0)), Mode: "enforcing"},
		{Event: plangateaudit.EventCardBuilt, PlanDigest: "d1", PhaseIndex: new(int32(1)), Outcome: plangateaudit.OutcomeDenied, Mode: "enforcing"},
		// phase 1's card_built stays denied: no matching phase_approved follows.
	}}

	yes, no, _, _ := steelthread.DeriveApprovalSettings(recs)
	assert.Empty(t, yes, "plan_phase was denied at least once, so it must not be auto-approved")
	assert.Equal(t, []string{"plan_phase"}, no)
}

// TestDeriveApprovalSettings_AutoApproveIsSortedNotInsertionOrder covers both
// plan-gate categories resolving to AutoApprove in the same run. Amendment
// events are processed before phase events in the Gate slice here, so a
// passing result proves the output is genuinely SORTED ("plan_amendment" <
// "plan_phase") rather than merely reflecting whatever order the two
// categories happened to resolve in.
func TestDeriveApprovalSettings_AutoApproveIsSortedNotInsertionOrder(t *testing.T) {
	recs := steelthread.Records{Gate: []plangateaudit.Content{
		{Event: plangateaudit.EventAmendmentRequested, PlanDigest: "d1", PhaseIndex: new(int32(0)), Handle: "perm:write:tracker_issue", Outcome: plangateaudit.OutcomeDenied, Mode: "enforcing"},
		{Event: plangateaudit.EventPhaseApproved, PlanDigest: "d1", PhaseIndex: new(int32(0)), Handle: "perm:write:tracker_issue", Mode: "enforcing"},
		{Event: plangateaudit.EventCardBuilt, PlanDigest: "d1", PhaseIndex: new(int32(0)), Outcome: plangateaudit.OutcomeDenied, Mode: "enforcing"},
		{Event: plangateaudit.EventPhaseApproved, PlanDigest: "d1", PhaseIndex: new(int32(0)), Mode: "enforcing"},
	}}

	yes, no, _, _ := steelthread.DeriveApprovalSettings(recs)
	assert.Equal(t, []string{"plan_amendment", "plan_phase"}, yes)
	assert.Empty(t, no)
}

// TestDeriveApprovalSettings_LoggingModeNeverResolvedIsNotADecision covers a
// plan-gate record from LOGGING mode: the ask is recorded (CardJSON, for the
// dataset) but nobody was ever asked, so its Outcome stays the pessimistic
// default the write stamped it with and no later phase_approved record ever
// arrives. That must not read as a denial — nothing was decided.
func TestDeriveApprovalSettings_LoggingModeNeverResolvedIsNotADecision(t *testing.T) {
	recs := steelthread.Records{Gate: []plangateaudit.Content{
		{Event: plangateaudit.EventCardBuilt, PlanDigest: "d1", PhaseIndex: new(int32(0)), Mode: "logging"},
	}}

	yes, no, as, undetermined := steelthread.DeriveApprovalSettings(recs)
	assert.Empty(t, yes)
	assert.Empty(t, no)
	assert.Empty(t, as)
	assert.Empty(t, undetermined)
}

// TestDeriveApprovalSettings_NoApprovalsIsEmpty is the negative control: an
// unattended session must not acquire an autoApprove list it never needed.
func TestDeriveApprovalSettings_NoApprovalsIsEmpty(t *testing.T) {
	yes, no, as, undetermined := steelthread.DeriveApprovalSettings(steelthread.Records{})
	assert.Empty(t, yes)
	assert.Empty(t, no)
	assert.Empty(t, as)
	assert.Empty(t, undetermined)
}

// TestDeriveApprovalSettings_ApproverIsFrequencyThenAlphaTiebroken covers the
// multi-approver case AutoApproveAs cannot express exactly (it is one
// string): the identity who decided the most wins, and a tie breaks
// alphabetically so a re-capture of the same session is byte-identical
// rather than depending on map iteration order.
//
// "user:YWxpY2U" sorts alphabetically BEFORE "user:Ym9i" ('W' < 'm'), so the
// first case below is chosen specifically to make the alphabetically-first
// identity the LOSER — that is what proves frequency is actually consulted,
// rather than the tiebreak alone happening to produce the right answer.
func TestDeriveApprovalSettings_ApproverIsFrequencyThenAlphaTiebroken(t *testing.T) {
	cases := []struct {
		name      string
		approvals map[string]approval.Pair
		want      string
	}{
		{
			name: "frequency wins even though the loser sorts first",
			approvals: map[string]approval.Pair{
				"tu-1": {Outcome: &approval.Outcome{Decision: "approved", Approver: "user:Ym9i"}},
				"tu-2": {Outcome: &approval.Outcome{Decision: "denied", Approver: "user:Ym9i"}},
				"tu-3": {Outcome: &approval.Outcome{Decision: "approved", Approver: "user:YWxpY2U"}},
			},
			want: "user:Ym9i",
		},
		{
			name: "equal counts break alphabetically",
			approvals: map[string]approval.Pair{
				"tu-1": {Outcome: &approval.Outcome{Decision: "approved", Approver: "user:Ym9i"}},
				"tu-2": {Outcome: &approval.Outcome{Decision: "approved", Approver: "user:YWxpY2U"}},
			},
			want: "user:YWxpY2U",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, as, undetermined := steelthread.DeriveApprovalSettings(steelthread.Records{Approvals: tc.approvals})
			assert.Equal(t, tc.want, as)
			assert.Len(t, undetermined, len(tc.approvals))
		})
	}
}

// TestDeriveApprovalSettings_UndecidedApprovalIsNotUndetermined covers a
// request whose outcome never arrived (a truncated capture, or a session
// captured mid-pause). It must not appear in `undetermined` — there is
// nothing to report a finding about, since nothing was ever decided.
func TestDeriveApprovalSettings_UndecidedApprovalIsNotUndetermined(t *testing.T) {
	recs := steelthread.Records{Approvals: map[string]approval.Pair{
		"tu-1": {Request: &approval.Request{ToolName: "tracker_update_issue"}},
	}}
	_, _, as, undetermined := steelthread.DeriveApprovalSettings(recs)
	assert.Empty(t, as)
	assert.Empty(t, undetermined)
}
