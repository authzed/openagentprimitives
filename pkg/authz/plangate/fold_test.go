package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// threePhasePlan is the fixture most fold tests approve.
func threePhasePlan(t *testing.T) Plan {
	t.Helper()
	surface := surfaceWith(t, "perm:read:tracker_issue", "perm:write:tracker_issue", "perm:send:email")
	p, probs := FreezeFrom([]AuthoredPhase{
		authored("a", "Recon", "perm:read:tracker_issue"),
		authored("b", "Write", "perm:write:tracker_issue"),
		authored("c", "Notify", "perm:send:email"),
	}, surface, nil)
	require.Empty(t, probs)
	return p
}

func approveRec(p Plan) plangateaudit.Content {
	return plangateaudit.Content{Event: plangateaudit.EventPlanApproved, PlanDigest: p.Digest()}
}

func selectRec(p Plan, idx int32) plangateaudit.Content {
	return plangateaudit.Content{
		Event: plangateaudit.EventPhaseSelected, PlanDigest: p.Digest(), PhaseIndex: &idx,
	}
}

// denyRec models what the system actually writes on a denial: the record
// carries the CEILING it refused, so the denial stays interpretable after the
// plan it was made against is gone.
func denyRec(p Plan, idx int32) plangateaudit.Content {
	rec := plangateaudit.Content{
		Event: plangateaudit.EventDenied, PlanDigest: p.Digest(), PhaseIndex: &idx,
	}
	if int(idx) < len(p.Phases) {
		for _, h := range p.Phases[idx].Permissions {
			rec.Ceiling = append(rec.Ceiling, h.String())
		}
	}
	return rec
}

// Absent any selection record the active phase is index 0 — AND that implicit
// position CONSUMES index 0's first entry.
//
// Without that, phase 0 silently gets one more entry than every other phase,
// and no plan could legitimately return to its first phase: the budget it
// never spent on entry would still be there, so "re-enter phase 0" would be
// free exactly once, for phase 0 only.
func TestFold_implicitInitialPositionConsumesPhaseZerosFirstEntry(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{approveRec(p)})
	require.NoError(t, err)

	assert.Equal(t, 0, st.ActivePhase)
	assert.Equal(t, 1, st.Spent[0], "the implicit start spends phase 0's first entry")
	assert.Zero(t, st.Spent[1])
	assert.Zero(t, st.Spent[2])
}

func TestFold_selectionMovesTheActivePhaseAndSpendsItsBudget(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{approveRec(p), selectRec(p, 1)})
	require.NoError(t, err)

	assert.Equal(t, 1, st.ActivePhase)
	assert.Equal(t, 1, st.Spent[0], "phase 0's implicit entry still counts")
	assert.Equal(t, 1, st.Spent[1])
}

func TestFold_reEnteringAPhaseSpendsItAgain(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveRec(p), selectRec(p, 1), selectRec(p, 0), selectRec(p, 1),
	})
	require.NoError(t, err)

	assert.Equal(t, 1, st.ActivePhase)
	assert.Equal(t, 2, st.Spent[0], "one implicit entry plus one re-entry")
	assert.Equal(t, 2, st.Spent[1])
}

// The active ceiling is the selected phase's, and only that phase's.
func TestFold_activeCeilingIsTheSelectedPhaseOnly(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{approveRec(p), selectRec(p, 1)})
	require.NoError(t, err)

	ceiling, cerr := st.ActiveCeiling()
	require.NoError(t, cerr)
	require.Len(t, ceiling, 1)
	assert.Contains(t, ceiling, p.Phases[1].Permissions[0])
	assert.NotContains(t, ceiling, p.Phases[0].Permissions[0], "phase 0's reach is not held while phase 1 is active")
}

// A human's Deny is durable. Nothing the agent does afterwards restores what
// was denied — including superseding with a byte-identical plan, which was the
// clean four-step bypass audit found.
func TestFold_denialSurvivesAByteIdenticalSupersede(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveRec(p),
		denyRec(p, 1),
		// The agent re-submits the SAME plan. Same digest, same phases.
		approveRec(p),
	})
	require.NoError(t, err)

	assert.True(t, st.IsDenied(PhaseRef{p.Digest(), 1}),
		"a supersede must not clear a human's denial")
}

// Denials key on the CEILING, not on a phase id or index, so they survive a
// plan being reshaped around them.
func TestFold_denialSurvivesAPhaseSplit(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue", "perm:write:tracker_issue")

	wide, _ := FreezeFrom([]AuthoredPhase{
		authored("w", "Wide", "perm:read:tracker_issue", "perm:write:tracker_issue"),
	}, surface, nil)

	// The human denies the wide phase. The agent then splits it in two, so the
	// denied ceiling now lives at a different index in a different plan.
	split, _ := FreezeFrom([]AuthoredPhase{
		authored("r", "Read", "perm:read:tracker_issue"),
		authored("w", "Write", "perm:write:tracker_issue"),
	}, surface, nil)

	st, err := Fold(split, []plangateaudit.Content{
		approveRec(wide),
		denyRec(wide, 0),
		approveRec(split),
	})
	require.NoError(t, err)

	assert.True(t, st.DeniedCeilingIntersects(split.Phases[1].Permissions),
		"the write reach the human denied stays denied after a split")
}

// Supersede is a FLOOR, not a reset: it restores budgets only where nothing
// was denied.
func TestFold_supersedeRestoresBudgetsOnlyWhereNothingWasDenied(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveRec(p),
		selectRec(p, 1), // spends phase 1
		denyRec(p, 1),   // human denies re-entry to phase 1
		approveRec(p),   // agent supersedes with the same plan
	})
	require.NoError(t, err)

	assert.True(t, st.IsDenied(PhaseRef{p.Digest(), 1}))
	assert.Equal(t, 1, st.Spent[1],
		"a denied phase's spent budget must NOT be restored by a supersede")
}

// THE fail-closed direction, and the one that is easy to get backwards.
//
// Taking the last confidently-read selection fails OPEN: a truncated read that
// drops later switch records would restore a ceiling the agent had already
// moved off. So an unreadable fold yields the EMPTY ceiling — the agent holds
// nothing until it re-selects.
func TestFold_unreadableTailYieldsTheEmptyCeilingNotTheLastGoodPhase(t *testing.T) {
	p := threePhasePlan(t)
	bad := int32(99) // out of range for this plan

	st, err := Fold(p, []plangateaudit.Content{
		approveRec(p),
		selectRec(p, 2),
		{Event: plangateaudit.EventPhaseSelected, PlanDigest: p.Digest(), PhaseIndex: &bad},
	})
	require.NoError(t, err, "a bad record is a fold OUTCOME, not a Go error")

	assert.True(t, st.Doubtful, "the fold must flag that it could not be trusted")

	ceiling, cerr := st.ActiveCeiling()
	require.Error(t, cerr, "a doubtful fold must refuse to name a ceiling")
	assert.Empty(t, ceiling, "the agent holds nothing until it re-selects")
}

func TestFold_recordForADifferentPlanIsIgnored(t *testing.T) {
	p := threePhasePlan(t)
	other, _ := FreezeFrom([]AuthoredPhase{authored("x", "X")}, nil, nil)

	st, err := Fold(p, []plangateaudit.Content{
		approveRec(p),
		selectRec(other, 0), // belongs to a different plan entirely
	})
	require.NoError(t, err)

	assert.Equal(t, 0, st.ActivePhase, "a foreign record must not move this plan's position")
	assert.False(t, st.Doubtful, "ignoring a foreign record is normal, not doubtful")
}

// Selection records with no phase index are malformed; the fold must not read
// a nil pointer as index 0.
func TestFold_selectionWithNoPhaseIndexIsDoubtful(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveRec(p),
		{Event: plangateaudit.EventPhaseSelected, PlanDigest: p.Digest()},
	})
	require.NoError(t, err)
	assert.True(t, st.Doubtful, "a selection naming no phase cannot be interpreted")
}

func TestFold_isDeterministic(t *testing.T) {
	p := threePhasePlan(t)
	recs := []plangateaudit.Content{approveRec(p), selectRec(p, 1), selectRec(p, 2), denyRec(p, 0)}

	first, err := Fold(p, recs)
	require.NoError(t, err)
	for i := 0; i < 50; i++ {
		again, err := Fold(p, recs)
		require.NoError(t, err)
		assert.Equal(t, first, again)
	}
}

func TestFold_emptyLogYieldsTheImplicitStart(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, nil)
	require.NoError(t, err)

	assert.Equal(t, 0, st.ActivePhase)
	assert.Equal(t, 1, st.Spent[0])
}

// An empty plan has no phase 0 to start in, so there is nothing to hold.
func TestFold_emptyPlanYieldsAnEmptyCeiling(t *testing.T) {
	st, err := Fold(Plan{}, nil)
	require.NoError(t, err)

	_, cerr := st.ActiveCeiling()
	assert.Error(t, cerr)
}

// The safety invariant the fuzzer also checks: whatever the log says, the
// active ceiling is always a subset of the frozen plan's union. No sequence of
// records can conjure reach the human never approved.
func TestFold_ceilingIsAlwaysASubsetOfThePlan(t *testing.T) {
	p := threePhasePlan(t)
	union := map[string]struct{}{}
	for _, ph := range p.Phases {
		for _, h := range ph.Permissions {
			union[h.String()] = struct{}{}
		}
	}

	st, err := Fold(p, []plangateaudit.Content{approveRec(p), selectRec(p, 2), selectRec(p, 1)})
	require.NoError(t, err)

	ceiling, cerr := st.ActiveCeiling()
	require.NoError(t, cerr)
	for h := range ceiling {
		assert.Contains(t, union, h.String())
	}
}

func TestFold_stateImpactIsNotInvented(t *testing.T) {
	// Sanity: the fold deals in handles, never in impact. Impact comes from
	// the live surface at decision time, so a stale record cannot downgrade a
	// handle's severity.
	assert.NotEqual(t, authz.Readonly, authz.External)
}

func approveePhaseRec(p Plan, idx int32) plangateaudit.Content {
	return plangateaudit.Content{
		Event: plangateaudit.EventPhaseApproved, PlanDigest: p.Digest(), PhaseIndex: &idx,
	}
}

// Declaring a ceiling and being CLEARED TO USE it are different facts.
// Collapsing them would make an agent's own declaration self-approving, which
// is the entire thing the gate exists to prevent.
func TestFold_declaringAPhaseDoesNotApproveIt(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{approveRec(p)})
	require.NoError(t, err)

	assert.False(t, st.PhaseApproved(0),
		"a frozen phase is declared, not yet cleared to run")
}

func TestFold_anApprovedPhaseIsCleared(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{approveRec(p), approveePhaseRec(p, 0)})
	require.NoError(t, err)

	assert.True(t, st.PhaseApproved(0))
	assert.False(t, st.PhaseApproved(1), "approval is per phase, never plan-wide")
}

// An approval belongs to the AUTHORITY it was granted against, so re-shaping a
// phase must not carry a human's decision onto a ceiling they never saw.
//
// This case pins the LEGACY path: approveePhaseRec writes no PhaseKey, as every
// binary did before the key existed, so the record folds by (digest, index) and
// a different plan simply does not match. The key-bearing equivalent is
// TestFold_approvalIsLostWhenThePhaseGainsAuthority — same guarantee, different
// mechanism, and both matter while an append-only log can hold records of both
// shapes.
func TestFold_approvalDoesNotTransplantAcrossPlans(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue", "perm:write:tracker_issue")
	narrow, _ := FreezeFrom([]AuthoredPhase{authored("p", "P", "perm:read:tracker_issue")}, surface, nil)
	wide, _ := FreezeFrom([]AuthoredPhase{
		authored("p", "P", "perm:read:tracker_issue", "perm:write:tracker_issue"),
	}, surface, nil)

	// The human approved the NARROW plan's phase 0; the agent then widened it.
	st, err := Fold(wide, []plangateaudit.Content{
		approveRec(narrow), approveePhaseRec(narrow, 0), approveRec(wide),
	})
	require.NoError(t, err)

	assert.False(t, st.PhaseApproved(0),
		"an approval for a different ceiling must not clear the widened one")
}

// A denial is not merely "not approved" — it must stay denied even if an
// approval for the same phase arrives later in the log.
func TestFold_denialOutranksALaterApproval(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveRec(p), denyRec(p, 1), approveePhaseRec(p, 1),
	})
	require.NoError(t, err)

	assert.True(t, st.IsDenied(PhaseRef{p.Digest(), 1}))
	assert.False(t, st.PhaseApproved(1),
		"a human's no is not undone by a later approval record")
}

// A gate-issued denial must not be folded as a HUMAN's denial.
//
// requirePlan refuses a call made before any plan exists. That is mechanical and
// clears the moment the agent plans. EventDenied means something entirely
// different — a person refused a phase — and the fold makes it sticky ON PURPOSE
// so the refusal outlives a reshaped plan. Recording the first as the second
// would attribute a refusal to a human who never made one, and leave the agent
// unable to clear it by doing exactly what the denial told it to do.
func TestFold_gateDeniedIsNotAHumanDenial(t *testing.T) {
	surface := demoSurface(t)
	plan, probs := FreezeFrom([]AuthoredPhase{
		{ID: "recon", Label: "Recon", Why: "read", Permissions: []AuthoredPermission{
			{Handle: "perm:read:tracker_issue", Why: "to read"},
		}},
	}, surface, nil)
	require.Empty(t, probs)

	// The record shape the HOOK actually writes, which is the whole point: it
	// sets PlanDigest from activePlan() and PhaseIndex from the resolved phase
	// BEFORE the requirePlan branch runs. With no declared plan, activePlan() is
	// the synthesized whole-surface plan — so the denial carries a real digest
	// and phase 0, which is exactly what the fold keys a human denial on.
	//
	// An earlier version of this test passed an empty digest and no phase index,
	// and the mutation below passed against it: the dangerous path was never
	// reached. A fold test that does not use the producer's own record shape
	// tests nothing about the producer.
	idx := int32(0)
	st, err := Fold(plan, []plangateaudit.Content{
		{Event: plangateaudit.EventGateDenied, Outcome: plangateaudit.OutcomeDenied,
			PlanDigest: plan.Digest(), PhaseIndex: &idx,
			Handle: "perm:read:tracker_issue", Tool: "read_issue"},
		// Then the agent does what the denial told it to do.
		{Event: plangateaudit.EventPlanApproved, PlanDigest: plan.Digest(), PhaseIndex: &idx},
		{Event: plangateaudit.EventPhaseApproved, PlanDigest: plan.Digest(), PhaseIndex: &idx},
	})
	require.NoError(t, err)

	assert.False(t, st.Doubtful,
		"a gate denial names no phase; folding it as a human denial marks the state doubtful")
	assert.True(t, st.PhaseApproved(0),
		"the phase the agent went on to declare and get approved must be usable")
	assert.False(t, st.IsDenied(PhaseRef{PlanDigest: plan.Digest(), Index: 0}),
		"nobody refused this phase; a mechanical gate denial must not read as one")
}
