package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProjectAwaitingDecisionSetsCondition(t *testing.T) {
	s := State{Phase: PhaseAwaitingDecision, Pending: []PendingDecision{
		{RequestID: "r1", Kind: DecisionContentInspect}}}
	v := Project(s)
	assert.Equal(t, "AwaitingDecision", v.Phase)
	var found bool
	for _, c := range v.Conditions {
		if c.Type == "ContentInspectionApprovalPending" && c.Status == "True" {
			found = true
		}
	}
	assert.True(t, found, "content_inspection pending → ContentInspectionApprovalPending=True")
}

func TestProjectSurfacesGated(t *testing.T) {
	v := Project(State{Phase: PhaseRunning, Gated: 3})
	assert.Equal(t, 3, v.ToolCallGated, "hook denies are surfaced (no-silent-errors)")
}

// TestProjectWatchdogGranularity confirms PendingByKind preserves the per-kind
// distinction between leakage_share, tool_call, and join. All three put the
// session in AwaitingDecision, but channelsd's watchdog selects a different
// silence budget for each (WaitLeakage vs WaitVisible). Coarsening to the
// phase alone would break that selection.
func TestProjectWatchdogGranularity(t *testing.T) {
	s := State{
		Phase: PhaseAwaitingDecision,
		Pending: []PendingDecision{
			{RequestID: "tc1", Kind: DecisionToolCall},
			{RequestID: "ls1", Kind: DecisionLeakageShare},
			{RequestID: "j1", Kind: DecisionJoin},
		},
	}
	v := Project(s)
	assert.Equal(t, []string{"tc1"}, v.PendingByKind[DecisionToolCall],
		"tool_call request IDs must be present under their own kind key")
	assert.Equal(t, []string{"ls1"}, v.PendingByKind[DecisionLeakageShare],
		"leakage_share request IDs must be present under their own kind key")
	assert.Equal(t, []string{"j1"}, v.PendingByKind[DecisionJoin],
		"join request IDs must be present under their own kind key")
	// None of the slices must bleed into each other — this would corrupt watchdog policy selection.
	assert.NotEqual(t, v.PendingByKind[DecisionToolCall], v.PendingByKind[DecisionLeakageShare],
		"tool_call and leakage_share must remain distinct in PendingByKind")
	assert.NotEqual(t, v.PendingByKind[DecisionJoin], v.PendingByKind[DecisionLeakageShare],
		"join and leakage_share must remain distinct in PendingByKind")
}

// TestProjectConditionsDeterministic confirms that Project produces Conditions
// in a stable sorted order regardless of map-iteration order. Without the
// sort.Slice in Project, two pending kinds from different map slots can produce
// reversed slices on successive calls, thrashing CR .status.conditions every
// reconcile even when nothing changed.
func TestProjectConditionsDeterministic(t *testing.T) {
	s := State{
		Phase: PhaseAwaitingDecision,
		Pending: []PendingDecision{
			{RequestID: "tc1", Kind: DecisionToolCall},
			{RequestID: "ls1", Kind: DecisionLeakageShare},
		},
	}
	v1 := Project(s)
	v2 := Project(s)
	assert.Equal(t, v1.Conditions, v2.Conditions,
		"Project must return Conditions in identical order on repeated calls (sort by Type)")
	// Confirm both expected condition types are present and in sorted order.
	if assert.Len(t, v1.Conditions, 2, "expect exactly two conditions for tool_call+leakage_share") {
		assert.Equal(t, "InfoLeakageApprovalPending", v1.Conditions[0].Type,
			"InfoLeakageApprovalPending sorts before ToolApprovalPending")
		assert.Equal(t, "ToolApprovalPending", v1.Conditions[1].Type,
			"ToolApprovalPending sorts after InfoLeakageApprovalPending")
	}
}

func TestProjectPhasePassthrough(t *testing.T) {
	cases := []struct {
		phase     Phase
		wantPhase string
	}{
		{PhasePending, "Pending"},
		{PhaseRunning, "Running"},
		{PhaseIdle, "Idle"},
		{PhaseSucceeded, "Succeeded"},
		{PhaseFailed, "Failed"},
		{PhaseAwaitingRetry, "AwaitingRetry"},
		{PhaseAwaitingCredentials, "AwaitingCredentials"},
		{PhaseAwaitingDecision, "AwaitingDecision"},
	}
	for _, tc := range cases {
		t.Run(string(tc.phase), func(t *testing.T) {
			v := Project(State{Phase: tc.phase})
			assert.Equal(t, tc.wantPhase, v.Phase)
		})
	}
}

func TestProjectFailureReason(t *testing.T) {
	v := Project(State{Phase: PhaseFailed, FailureReason: "BudgetExceeded"})
	assert.Equal(t, "BudgetExceeded", v.FailureReason)
}

func TestProjectAwaitingUserInput(t *testing.T) {
	v := Project(State{Phase: PhaseRunning, AwaitingUserInput: true})
	assert.True(t, v.AwaitingUserInput)
}

func TestProjectScopeReviewPending(t *testing.T) {
	s := State{
		Phase:              PhaseAwaitingDecision,
		ScopeReviewPending: true,
		Pending: []PendingDecision{
			{RequestID: "sr1", Kind: DecisionScopeReview},
		},
	}
	v := Project(s)
	assert.True(t, v.ScopeReviewPending)
	var found bool
	for _, c := range v.Conditions {
		if c.Type == "ScopeReviewPending" && c.Status == "True" {
			found = true
		}
	}
	assert.True(t, found, "scope_review pending → ScopeReviewPending condition True")
}

func TestProjectToolApprovalPending(t *testing.T) {
	s := State{
		Phase: PhaseAwaitingDecision,
		Pending: []PendingDecision{
			{RequestID: "ta1", Kind: DecisionToolCall},
		},
	}
	v := Project(s)
	var found bool
	for _, c := range v.Conditions {
		if c.Type == "ToolApprovalPending" && c.Status == "True" {
			found = true
		}
	}
	assert.True(t, found, "tool_call pending → ToolApprovalPending=True")
}

func TestProjectPermissionRequestPending(t *testing.T) {
	s := State{
		Phase: PhaseAwaitingDecision,
		Pending: []PendingDecision{
			{RequestID: "j1", Kind: DecisionJoin},
		},
	}
	v := Project(s)
	var found bool
	for _, c := range v.Conditions {
		if c.Type == "PermissionRequestPending" && c.Status == "True" {
			found = true
		}
	}
	assert.True(t, found, "join pending → PermissionRequestPending=True")
}

func TestProjectGatedCondition(t *testing.T) {
	v := Project(State{Phase: PhaseRunning, Gated: 2})
	var found bool
	for _, c := range v.Conditions {
		if c.Type == "ToolCallGated" && c.Status == "True" {
			found = true
		}
	}
	assert.True(t, found, "gated > 0 → ToolCallGated condition True")
}

func TestProjectNoPendingNoConditions(t *testing.T) {
	v := Project(State{Phase: PhaseRunning})
	assert.Empty(t, v.Conditions)
	assert.Nil(t, v.PendingByKind)
}
