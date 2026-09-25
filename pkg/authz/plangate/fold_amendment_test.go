package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// amendRec models what an approved plan_amendment actually writes: a
// PhaseApproved event naming the ONE handle the amendment added, an index, and
// an EMPTY PhaseKey — the amendment ask's payload carries no phaseKey, so
// host_approval has nothing to put there.
func amendRec(p Plan, idx int32, handle string) plangateaudit.Content {
	return plangateaudit.Content{
		Event:      plangateaudit.EventPhaseApproved,
		PlanDigest: p.Digest(),
		PhaseIndex: &idx,
		Handle:     handle,
	}
}

// Approving a narrow AMENDMENT must not silently approve the entire
// un-approved PHASE.
//
// The record an approved amendment writes is an EventPhaseApproved carrying a
// PhaseIndex and an empty PhaseKey. The key-bearing short-circuit is guarded on
// PhaseKey != "", so it did not fire; the amendment branch recorded the added
// handle and then FELL THROUGH — with no continue — into the legacy
// whole-phase approval map, whose own comment asserts "anything reaching here
// is LEGACY: written before PhaseKey existed." An amendment is not legacy.
//
// The escape it opened: declare a phase whose ceiling holds readwrite/external
// handles, so it is priced tier 1/2 and freeze writes no tier-0 clearance —
// the phase is un-approved. Then issue one call whose handle is deliberately
// OUTSIDE that ceiling. phaseNeedsApproval is only reached inside the
// in-ceiling arm, so the phase is never challenged; the out-of-ceiling arm
// raises a plan_amendment instead. The human is shown one added handle, under
// a card that says the plan is already approved and that wipes the phase's
// real ceiling before rendering. One click, and every readwrite/external
// handle in that phase dispatches with no card.
func TestFold_ApprovingAnAmendmentDoesNotApproveTheWholePhase(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveRec(p),
		selectRec(p, 1),
		// The human approved ONE added handle on phase 1 — not phase 1.
		amendRec(p, 1, "perm:read:tracker_issue"),
	})
	require.NoError(t, err)

	assert.False(t, st.PhaseApproved(1),
		"an amendment approves the handle it named, not the phase that was never challenged")

	// And the amendment's own effect must survive: the added handle is in the
	// active ceiling, which is the whole point of recording it.
	ceiling, cerr := st.ActiveCeiling()
	require.NoError(t, cerr)
	var found bool
	for h := range ceiling {
		if h.String() == "perm:read:tracker_issue" {
			found = true
		}
	}
	assert.True(t, found, "the widening the human DID approve must outlive the call that triggered it")
}

// The legacy shape must keep folding exactly as it did. This log is
// append-only: a record written before PhaseKey existed carries no key AND no
// handle, and a session upgraded mid-run must not start re-asking for work the
// user already cleared.
func TestFold_AHandlelessApprovalStillApprovesThePhase(t *testing.T) {
	p := threePhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveRec(p),
		selectRec(p, 1),
		approveePhaseRec(p, 1), // no PhaseKey, no Handle: the legacy whole-phase approval
	})
	require.NoError(t, err)

	assert.True(t, st.PhaseApproved(1),
		"a handle-less, key-less approval is the legacy whole-phase grant and must keep folding as one")
}
