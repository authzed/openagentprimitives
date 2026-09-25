package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// clearRec models what the operator writes on releasing a forensic hold: the
// clear names the plan digest whose approvals must drop, exactly like a
// supersede or a plan-approval record does.
func clearRec(p Plan) plangateaudit.Content {
	return plangateaudit.Content{Event: plangateaudit.EventApprovalsCleared, PlanDigest: p.Digest()}
}

// supersedeRec models the agent re-approving a (possibly byte-identical)
// plan, which is the FLOOR path this test file's guard rail distinguishes
// from a clear.
func supersedeRec(p Plan) plangateaudit.Content {
	return plangateaudit.Content{Event: plangateaudit.EventSuperseded, PlanDigest: p.Digest()}
}

// approveKeyRec models the MODERN approval path: PhaseApproved checks
// approvedKeys (keyed by Phase.AuthorityKey, digest-independent) before it
// ever falls through to the legacy approvedPhases form — see
// State.PhaseApproved. A record carrying a non-empty PhaseKey never touches
// approvedPhases at all (fold.go's key-bearing branch `continue`s before the
// foreign-plan skip).
func approveKeyRec(p Plan, idx int32) plangateaudit.Content {
	return plangateaudit.Content{
		Event: plangateaudit.EventPhaseApproved, PlanDigest: p.Digest(),
		PhaseIndex: &idx, PhaseKey: p.Phases[idx].AuthorityKey(),
	}
}

func TestFold_approvalsClearedEmptiesApprovedPhases(t *testing.T) {
	p := twoPhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveePhaseRec(p, 0),
		approveePhaseRec(p, 1),
		clearRec(p),
	})
	require.NoError(t, err)

	assert.False(t, st.PhaseApproved(0),
		"release must drop standing approvals so the agent re-plans")
	assert.False(t, st.PhaseApproved(1))
}

func TestFold_approvalsClearedYieldsEmptyCeiling(t *testing.T) {
	p := twoPhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveePhaseRec(p, 0),
		clearRec(p),
		selectRec(p, 0),
	})
	require.NoError(t, err)

	assert.False(t, st.PhaseApproved(0),
		"re-selecting a phase whose approval was cleared must hold nothing until it is re-approved")
}

func TestFold_supersededDoesNotClearApprovals(t *testing.T) {
	// Pins the distinction that motivated EventApprovalsCleared. Supersede is a
	// FLOOR, not a reset: collapsing the two would silently let a released
	// agent resume on its old ceiling.
	p := twoPhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveePhaseRec(p, 0),
		supersedeRec(p),
	})
	require.NoError(t, err)

	assert.True(t, st.PhaseApproved(0),
		"supersede restores budgets; it must NOT drop a human's approval")
}

// A clear must reach the approval however it was recorded. A key-bearing
// approval is the MODERN path — every current write goes through PhaseKey,
// and approvedPhases (the legacy digest+index form) is what predates it — so
// a clear that only empties approvedPhases is a no-op against the approvals
// sessions actually hold today.
func TestFold_approvalsClearedDropsAKeyBearingApproval(t *testing.T) {
	p := twoPhasePlan(t)

	st, err := Fold(p, []plangateaudit.Content{
		approveKeyRec(p, 0),
		clearRec(p),
	})
	require.NoError(t, err)

	assert.False(t, st.PhaseApproved(0),
		"release must drop a modern, key-bearing approval too, not only the legacy form")
}
