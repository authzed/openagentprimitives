package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// planWithEdge: phase 1 requires phase 0; phase 2 requires nothing.
func planWithEdge(t *testing.T) Plan {
	t.Helper()
	return Plan{Phases: []Phase{
		{Label: "Recon", Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 1}},
		{Label: "Act", Permissions: hsIn(t, "perm:write:doc"), Max: MaxSpec{Count: 1},
			Requires: []RequiresEdge{{Phase: 0}}},
		{Label: "Report", Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 1}},
	}}
}

// Leaving a phase unfinished is ALLOWED — it is simply marked. An agent that
// discovers phase 1 is unnecessary should not be wedged into completing it, and
// forcing strict sequence would make every abandoned branch a dead session.
func TestBlockedByIncompletePrerequisite_anUnrelatedPhaseMayOpenWithWorkOutstanding(t *testing.T) {
	p := planWithEdge(t)

	// Phase 0 is incomplete; phase 2 does not require it.
	blocked, why := BlockedByIncompletePrerequisite(p, 2, map[int]bool{0: false})

	assert.False(t, blocked, "phase 2 has no dependency on phase 0; leaving it open is fine")
	assert.Empty(t, why)
}

// The one case that IS blocked: the target depends on a phase that has not
// finished. Opening it would run work whose prerequisite the plan says must be
// done first — which is exactly what the approver agreed to when they cleared
// the ordering edge.
func TestBlockedByIncompletePrerequisite_aDependentPhaseWaits(t *testing.T) {
	p := planWithEdge(t)

	blocked, why := BlockedByIncompletePrerequisite(p, 1, map[int]bool{0: false})

	assert.True(t, blocked)
	assert.Contains(t, why, "Recon", "the message must name the phase that is holding it up")
}

// Once the prerequisite is finished the dependent phase opens normally.
func TestBlockedByIncompletePrerequisite_aFinishedPrerequisiteReleasesIt(t *testing.T) {
	p := planWithEdge(t)

	blocked, _ := BlockedByIncompletePrerequisite(p, 1, map[int]bool{0: true})

	assert.False(t, blocked)
}

// A phase whose completion is UNKNOWN is treated as incomplete. The map carries
// what the runtime could establish, and absence means it could not establish
// anything — reading that as "finished" would open a dependent phase on no
// evidence at all.
func TestBlockedByIncompletePrerequisite_unknownCompletionCountsAsIncomplete(t *testing.T) {
	p := planWithEdge(t)

	blocked, _ := BlockedByIncompletePrerequisite(p, 1, nil)

	assert.True(t, blocked, "no evidence of completion is not evidence of completion")
}

// Several prerequisites: every one must be finished, and the message names the
// ones that are not.
func TestBlockedByIncompletePrerequisite_reportsEveryOutstandingPrerequisite(t *testing.T) {
	p := Plan{Phases: []Phase{
		{Label: "A", Max: MaxSpec{Count: 1}},
		{Label: "B", Max: MaxSpec{Count: 1}},
		{Label: "C", Max: MaxSpec{Count: 1}, Requires: []RequiresEdge{{Phase: 0}, {Phase: 1}}},
	}}

	blocked, why := BlockedByIncompletePrerequisite(p, 2, map[int]bool{0: true, 1: false})

	require.True(t, blocked)
	assert.Contains(t, why, "B")
	assert.NotContains(t, why, `"A"`, "A is done; naming it would send the agent to fix nothing")
}

// An out-of-range target is not a completion question — the caller's own bounds
// check owns that error, and answering here would replace a precise message
// with a misleading one about prerequisites.
func TestBlockedByIncompletePrerequisite_outOfRangeIsNotItsProblem(t *testing.T) {
	p := planWithEdge(t)

	blocked, _ := BlockedByIncompletePrerequisite(p, 99, nil)

	assert.False(t, blocked)
}

// completedRec is what the runtime writes when the agent declares a phase done.
func completedRec(p Plan, phase int, outcome string) plangateaudit.Content {
	idx := int32(phase)
	return plangateaudit.Content{
		Event: plangateaudit.EventPhaseCompleted, PlanDigest: p.Digest(),
		PhaseIndex: &idx, PhaseOutcome: outcome,
	}
}

func selectedRec(p Plan, phase int) plangateaudit.Content {
	idx := int32(phase)
	return plangateaudit.Content{
		Event: plangateaudit.EventPhaseSelected, PlanDigest: p.Digest(), PhaseIndex: &idx,
	}
}

// Completion is a RECORDED event, not a field in the agent's document. The
// agent still declares it — it always did — but the declaration lands in the
// append-only log, so it is ordered, monotonic, and cannot be un-declared by
// rewriting a document update_plan resubmits wholesale on every call.
func TestFold_completedPhasesComeFromTheLog(t *testing.T) {
	p := planWithEdge(t)
	st, err := Fold(p, []plangateaudit.Content{completedRec(p, 0, "listed the docs")})
	require.NoError(t, err)

	assert.True(t, st.CompletedPhases()[0], "phase 0 was entered implicitly and declared done")
	assert.False(t, st.CompletedPhases()[1])
}

// THE hazard: completion must not REPLACE the entry requirement.
//
// A requires edge is satisfied today by prior ENTRY, which is unforgeable —
// the runtime records the selection. If a completion record alone satisfied it,
// an agent could declare a phase it never entered complete and open the
// dependent phase, which is WEAKER than the rule it replaced. Completion has to
// be entry AND declaration, so it can only ever narrow.
func TestFold_completingAPhaseNeverEnteredDoesNotCountIt(t *testing.T) {
	p := planWithEdge(t)
	// Phase 2 was never selected, but the agent claims it is done.
	st, err := Fold(p, []plangateaudit.Content{completedRec(p, 2, "did it, honest")})
	require.NoError(t, err)

	assert.False(t, st.CompletedPhases()[2],
		"a phase the runtime never saw entered cannot be complete, whatever the agent says")
}

// Entering then completing is the ordinary path.
func TestFold_anEnteredThenCompletedPhaseCounts(t *testing.T) {
	p := planWithEdge(t)
	st, err := Fold(p, []plangateaudit.Content{
		selectedRec(p, 1), completedRec(p, 1, "made the edit"),
	})
	require.NoError(t, err)

	assert.True(t, st.CompletedPhases()[1])
}

// Completion is MONOTONIC. That is the property the plan document could not
// offer: an agent could mark an item done, open the dependent phase, and mark it
// pending again, and a document-reading gate would follow it back. A recorded
// completion is a point in time that persists.
func TestFold_completionSurvivesLaterRecords(t *testing.T) {
	p := planWithEdge(t)
	st, err := Fold(p, []plangateaudit.Content{
		selectedRec(p, 0), completedRec(p, 0, "done"), selectedRec(p, 0),
	})
	require.NoError(t, err)

	assert.True(t, st.CompletedPhases()[0], "re-entering a completed phase does not un-complete it")
}

// Another plan's completion records are another plan's evidence.
func TestFold_ignoresAnotherPlansCompletions(t *testing.T) {
	p := planWithEdge(t)
	other := Plan{Phases: []Phase{{Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 9}}}}
	require.NotEqual(t, p.Digest(), other.Digest())

	st, err := Fold(p, []plangateaudit.Content{completedRec(other, 0, "elsewhere")})
	require.NoError(t, err)

	assert.False(t, st.CompletedPhases()[0])
}
