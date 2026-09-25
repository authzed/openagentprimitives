package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// The four questions enforcement is gated on. Each subtest computes one from
// records ALONE — if a metric cannot be derived here, the record shape is
// wrong and the logging period would produce a dataset that cannot answer the
// question it exists to answer.
func TestDataset_answersTheSliceThreeGatingQuestions(t *testing.T) {
	p := threePhasePlan(t)
	idx0, idx1 := int32(0), int32(1)

	recs := []plangateaudit.Content{
		{Event: plangateaudit.EventPlanApproved, PlanDigest: p.Digest(), Tier: "1",
			PhaseCount: 3, HandleCount: 3},
		{Event: plangateaudit.EventGateAllowed, PlanDigest: p.Digest(), PhaseIndex: &idx0,
			Outcome: plangateaudit.OutcomeAllow, Handle: "perm:read:tracker_issue", Tool: "read_issue"},
		{Event: plangateaudit.EventGateAllowed, PlanDigest: p.Digest(), PhaseIndex: &idx0,
			Outcome: plangateaudit.OutcomeAllow, Handle: "perm:read:tracker_issue", Tool: "read_issue"},
		{Event: plangateaudit.EventGateWouldDeny, PlanDigest: p.Digest(), PhaseIndex: &idx0,
			Outcome: plangateaudit.OutcomeWouldDeny, Handle: "perm:write:tracker_issue", Tool: "update_issue",
			Tier: "1", Severity: "elevated"},
		{Event: plangateaudit.EventPhaseSelected, PlanDigest: p.Digest(), PhaseIndex: &idx1},
		{Event: plangateaudit.EventGateAllowed, PlanDigest: p.Digest(), PhaseIndex: &idx1,
			Outcome: plangateaudit.OutcomeAllow, Handle: "perm:write:tracker_issue", Tool: "update_issue"},
	}

	m := Metrics(p, recs)

	t.Run("1. would-be denial rate per session is computable", func(t *testing.T) {
		assert.Equal(t, 4, m.GatedCalls)
		assert.Equal(t, 1, m.WouldDeny)
		assert.InDelta(t, 0.25, m.WouldDenyRate(), 0.001)
	})

	t.Run("2. approval count by tier is computable", func(t *testing.T) {
		assert.Equal(t, 2, m.ApprovalsByTier["1"],
			"the plan approval and the would-be phase approval both priced at tier 1")
	})

	t.Run("3. over-declaration is computable", func(t *testing.T) {
		// Phase 2 (`perm:send:email`) was declared and never exercised.
		assert.Contains(t, m.DeclaredNeverExercised, "perm:send:email")
		assert.NotContains(t, m.DeclaredNeverExercised, "perm:read:tracker_issue")
		assert.Equal(t, 3, m.DeclaredHandles)
		assert.Equal(t, 2, m.ExercisedHandles)
	})

	t.Run("4. surface drift between approval and execution is computable", func(t *testing.T) {
		// Every handle exercised was one the plan declared, so no drift.
		assert.Empty(t, m.ExercisedNotDeclared)
	})
}

// Drift is the case that matters: a call whose handle the approved plan never
// declared. It means the surface changed between approval and execution, and
// enforcing mode would have denied it.
func TestMetrics_detectsSurfaceDrift(t *testing.T) {
	p := threePhasePlan(t)
	idx := int32(0)

	m := Metrics(p, []plangateaudit.Content{
		{Event: plangateaudit.EventGateWouldDeny, PlanDigest: p.Digest(), PhaseIndex: &idx,
			Outcome: plangateaudit.OutcomeWouldDeny, Handle: "perm:delete:everything", Tool: "nuke"},
	})

	assert.Contains(t, m.ExercisedNotDeclared, "perm:delete:everything")
}

func TestMetrics_emptyLogIsAllZeroesNotADivideByZero(t *testing.T) {
	m := Metrics(threePhasePlan(t), nil)

	assert.Zero(t, m.GatedCalls)
	assert.Zero(t, m.WouldDeny)
	assert.Zero(t, m.WouldDenyRate(), "no calls means no rate, not a panic")
}

// Records for other plans must not pollute this plan's numbers, or a session
// that superseded once would report a blended rate across two ceilings.
func TestMetrics_ignoresRecordsForOtherPlans(t *testing.T) {
	p := threePhasePlan(t)
	other, _ := FreezeFrom([]AuthoredPhase{authored("x", "X")}, nil, nil)
	idx := int32(0)

	m := Metrics(p, []plangateaudit.Content{
		{Event: plangateaudit.EventGateWouldDeny, PlanDigest: other.Digest(), PhaseIndex: &idx,
			Outcome: plangateaudit.OutcomeWouldDeny, Handle: "perm:read:tracker_issue"},
	})

	assert.Zero(t, m.GatedCalls)
}

// Calls with no handle (meta tools) are not gated calls and must not dilute the
// denial rate — including them would make every session look safer than it is.
func TestMetrics_callsWithNoHandleAreNotCounted(t *testing.T) {
	p := threePhasePlan(t)
	idx := int32(0)

	m := Metrics(p, []plangateaudit.Content{
		{Event: plangateaudit.EventGateAllowed, PlanDigest: p.Digest(), PhaseIndex: &idx,
			Outcome: plangateaudit.OutcomeAllow, Tool: "respond_to_user"},
	})

	assert.Zero(t, m.GatedCalls, "a call the gate does not govern is not a gated call")
}
