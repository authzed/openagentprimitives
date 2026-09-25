package hooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// twoPhaseGate builds a gate over a plan whose phase 0 can read and phase 1 can
// write, with the fold driven by the supplied records.
func twoPhaseGate(t *testing.T, rec *fakeRecorder, logs *fakeLogger, records []plangateaudit.Content) (*PlanGate, plangate.Plan) {
	t.Helper()

	read := permDesc(t, "read", "tracker_issue", "read_issue", authz.Readonly)
	write := permDesc(t, "write", "tracker_issue", "update_issue", authz.Readwrite)
	surface := []permsurface.Descriptor{read, write}

	plan, probs := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "recon", Label: "Recon", Why: "read first",
			Permissions: []plangate.AuthoredPermission{{Handle: read.Handle.String(), Why: "to read"}}},
		{ID: "write", Label: "Write", Why: "then write",
			Permissions: []plangate.AuthoredPermission{{Handle: write.Handle.String(), Why: "to write"}}},
	}, surface, nil)
	require.Empty(t, probs)

	h := NewPlanGate(PlanGateDeps{
		Mode:     "logging",
		Plan:     plan,
		Surface:  surface,
		Resolve:  surfaceResolver(t, surface),
		Records:  func() []plangateaudit.Content { return records },
		Recorder: rec,
		Logger:   logs,
	})
	return h, plan
}

func selected(plan plangate.Plan, idx int32) plangateaudit.Content {
	return plangateaudit.Content{
		Event: plangateaudit.EventPhaseSelected, PlanDigest: plan.Digest(), PhaseIndex: &idx,
	}
}

// The behavior a single whole-surface phase could not have: the SAME call
// records differently depending on which phase the fold says is active.
func TestPlanGate_ceilingFollowsTheActivePhase(t *testing.T) {
	t.Run("phase 0 active: the read is in ceiling, the write is not", func(t *testing.T) {
		rec := &fakeRecorder{}
		h, _ := twoPhaseGate(t, rec, &fakeLogger{}, nil) // no selection ⇒ implicit phase 0

		evalTool(t, h, "read_issue")
		assert.Equal(t, plangateaudit.OutcomeAllow, rec.last(t).Outcome)

		evalTool(t, h, "update_issue")
		assert.Equal(t, plangateaudit.OutcomeWouldDeny, rec.last(t).Outcome,
			"the write reach belongs to phase 1, which is not active")
	})

	t.Run("phase 1 active: the write is in ceiling, the read is not", func(t *testing.T) {
		rec := &fakeRecorder{}
		var plan plangate.Plan
		h, plan := twoPhaseGate(t, rec, &fakeLogger{}, nil)
		h = NewPlanGate(PlanGateDeps{
			Mode: "logging", Plan: plan,
			Surface:  h.deps.Surface,
			Resolve:  h.deps.Resolve,
			Records:  func() []plangateaudit.Content { return []plangateaudit.Content{selected(plan, 1)} },
			Recorder: rec, Logger: &fakeLogger{},
		})

		evalTool(t, h, "update_issue")
		assert.Equal(t, plangateaudit.OutcomeAllow, rec.last(t).Outcome)

		evalTool(t, h, "read_issue")
		assert.Equal(t, plangateaudit.OutcomeWouldDeny, rec.last(t).Outcome,
			"moving to phase 1 gives up phase 0's reach")
	})
}

// The record must name the phase the fold resolved, not a hardcoded 0, or the
// dataset cannot attribute a call to the ceiling that governed it.
func TestPlanGate_recordsTheFoldResolvedPhase(t *testing.T) {
	rec := &fakeRecorder{}
	h, plan := twoPhaseGate(t, rec, &fakeLogger{}, nil)
	h = NewPlanGate(PlanGateDeps{
		Mode: "logging", Plan: plan, Surface: h.deps.Surface, Resolve: h.deps.Resolve,
		Records:  func() []plangateaudit.Content { return []plangateaudit.Content{selected(plan, 1)} },
		Recorder: rec, Logger: &fakeLogger{},
	})

	evalTool(t, h, "update_issue")

	got := rec.last(t)
	require.NotNil(t, got.PhaseIndex)
	assert.Equal(t, int32(1), *got.PhaseIndex)
}

// Slice 2 is still logging: a would-deny under a genuinely NARROW ceiling —
// not the slice-1 whole-surface one — still never halts.
func TestPlanGate_narrowCeilingStillNeverHaltsUnderLogging(t *testing.T) {
	rec := &fakeRecorder{}
	h, _ := twoPhaseGate(t, rec, &fakeLogger{}, nil)

	d := evalTool(t, h, "update_issue")

	assert.Equal(t, plangateaudit.OutcomeWouldDeny, rec.last(t).Outcome)
	assert.Nil(t, d.Approval, "logging publishes nothing, however narrow the ceiling")
}

// A doubtful fold must be LOUD, must record, and — under logging — must still
// let the call through. Silence in either direction is the failure.
func TestPlanGate_doubtfulFoldLogsAndRecordsWithoutHalting(t *testing.T) {
	rec, logs := &fakeRecorder{}, &fakeLogger{}
	h, plan := twoPhaseGate(t, rec, logs, nil)

	bad := int32(99)
	h = NewPlanGate(PlanGateDeps{
		Mode: "logging", Plan: plan, Surface: h.deps.Surface, Resolve: h.deps.Resolve,
		Records: func() []plangateaudit.Content {
			return []plangateaudit.Content{{
				Event: plangateaudit.EventPhaseSelected, PlanDigest: plan.Digest(), PhaseIndex: &bad,
			}}
		},
		Recorder: rec, Logger: logs,
	})

	d := evalTool(t, h, "read_issue")

	assert.Equal(t, pipeline.Allow, d.Verdict, "logging never halts, even on a doubtful fold")
	assert.NotEmpty(t, logs.msgs, "a fold it could not trust must never be silent")
	assert.Equal(t, plangateaudit.OutcomeWouldDeny, rec.last(t).Outcome,
		"a doubtful fold yields the EMPTY ceiling, so every call is out of it")
}

// With no Records source the gate falls back to the plan's phase 0 rather than
// failing — this is the slice-1 shape and must keep working.
func TestPlanGate_noRecordsSourceUsesPhaseZero(t *testing.T) {
	rec := &fakeRecorder{}
	surface := demoSurface(t)
	h := NewPlanGate(PlanGateDeps{
		Mode: "logging", Plan: plangate.SessionPlan(surface), Surface: surface,
		Resolve: surfaceResolver(t, surface), Recorder: rec, Logger: &fakeLogger{},
	})

	evalTool(t, h, "read_issue")

	got := rec.last(t)
	require.NotNil(t, got.PhaseIndex)
	assert.Equal(t, int32(0), *got.PhaseIndex)
	assert.Equal(t, plangateaudit.OutcomeAllow, got.Outcome)
}
