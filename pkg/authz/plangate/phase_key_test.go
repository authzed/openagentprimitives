package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

func keyPlan(t *testing.T, handle, slotID string) Plan {
	t.Helper()
	p, probs := FreezeFrom([]AuthoredPhase{{
		ID: "p", Label: "Phase", Why: "w",
		Permissions: []AuthoredPermission{{Handle: handle, Why: "w"}},
		Slots:       []AuthoredSlot{{Type: "tracker_issue", ID: slotID, Why: "w"}},
	}}, demoSurface(t), []string{"tracker_issue"})
	require.Empty(t, probs)
	return p
}

// A phase's identity for APPROVAL purposes is its authority — the permissions it
// holds and the instances it names. Not its position, and not the plan it
// happens to sit in.
//
// Approval was keyed on PhaseRef{PlanDigest, Index}, so ANY re-plan invalidated
// EVERY approval, and reordering phases invalidated them even when nothing
// changed. Re-planning therefore re-charged the user for phases they had already
// read and approved — which is the opposite of "approve as much as possible,
// as early as possible".
func TestAuthorityKey_isStableAcrossUnrelatedPlanChanges(t *testing.T) {
	a := keyPlan(t, "perm:read:tracker_issue", "foo/bar")

	// A second plan whose phase carries the SAME authority but sits alongside
	// another phase — a different plan, a different digest, possibly a
	// different index.
	b, probs := FreezeFrom([]AuthoredPhase{
		{ID: "other", Label: "Other", Why: "w",
			Permissions: []AuthoredPermission{{Handle: "perm:write:tracker_issue", Why: "w"}}},
		{ID: "p", Label: "Renamed since", Why: "a different story",
			Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "w"}},
			Slots:       []AuthoredSlot{{Type: "tracker_issue", ID: "foo/bar", Why: "different why"}}},
	}, demoSurface(t), []string{"tracker_issue"})
	require.Empty(t, probs)

	assert.NotEqual(t, a.Digest(), b.Digest(), "different plans, as expected")
	assert.Equal(t, a.Phases[0].AuthorityKey(), b.Phases[1].AuthorityKey(),
		"same permissions and same named instance is the same authority — a re-plan "+
			"that leaves a phase alone must not re-charge the user for it")
}

// The converse, and the security half: anything that changes what the phase may
// DO must change its key, so a reshaped phase asks again.
func TestAuthorityKey_changesWhenAuthorityChanges(t *testing.T) {
	base := keyPlan(t, "perm:read:tracker_issue", "foo/bar").Phases[0].AuthorityKey()

	assert.NotEqual(t, base, keyPlan(t, "perm:write:tracker_issue", "foo/bar").Phases[0].AuthorityKey(),
		"a different permission is different authority")
	assert.NotEqual(t, base, keyPlan(t, "perm:read:tracker_issue", "baz/qux").Phases[0].AuthorityKey(),
		"a different INSTANCE is different authority — this is the reuse guard")
}

// Label and Why are agent-authored and display-only. A re-worded justification
// must not invalidate an approval: re-asking for the same authority under a new
// story is how users are trained to click through.
func TestAuthorityKey_ignoresAgentAuthoredText(t *testing.T) {
	mk := func(label, why string) string {
		p, probs := FreezeFrom([]AuthoredPhase{{
			ID: "p", Label: label, Why: why,
			Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: why}},
		}}, demoSurface(t), nil)
		require.Empty(t, probs)
		return p.Phases[0].AuthorityKey()
	}
	assert.Equal(t, mk("Read", "first story"), mk("Completely different label", "another story"),
		"the agent may re-word freely; it may not re-word its way into new authority")
}

// The point of the key: a re-plan that leaves a phase alone KEEPS its approval.
func TestFold_approvalSurvivesAReplanThatDidNotTouchThePhase(t *testing.T) {
	before := keyPlan(t, "perm:read:tracker_issue", "foo/bar")

	// Approved against the OLD plan, carrying the phase's authority key.
	idx := int32(0)
	records := []plangateaudit.Content{{
		Event: plangateaudit.EventPhaseApproved, PlanDigest: before.Digest(),
		PhaseIndex: &idx, PhaseKey: before.Phases[0].AuthorityKey(),
	}}

	// The agent re-plans: a new phase appears FIRST, so the old one moves to
	// index 1 and the plan digest changes. Its authority is untouched.
	after, probs := FreezeFrom([]AuthoredPhase{
		{ID: "new", Label: "New recon", Why: "w",
			Permissions: []AuthoredPermission{{Handle: "perm:write:tracker_issue", Why: "w"}}},
		{ID: "p", Label: "Phase", Why: "w",
			Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "w"}},
			Slots:       []AuthoredSlot{{Type: "tracker_issue", ID: "foo/bar", Why: "w"}}},
	}, demoSurface(t), []string{"tracker_issue"})
	require.Empty(t, probs)

	st, err := Fold(after, records)
	require.NoError(t, err)
	assert.True(t, st.PhaseApproved(1),
		"the phase moved and the plan digest changed, but its authority did not — "+
			"re-charging the user here is what the key exists to prevent")
	assert.False(t, st.PhaseApproved(0), "the NEW phase was never approved")
}

// A re-plan that WIDENS a phase must lose its approval. This is the half that
// keeps the key from being a bypass.
func TestFold_approvalIsLostWhenThePhaseGainsAuthority(t *testing.T) {
	before := keyPlan(t, "perm:read:tracker_issue", "foo/bar")
	idx := int32(0)
	records := []plangateaudit.Content{{
		Event: plangateaudit.EventPhaseApproved, PlanDigest: before.Digest(),
		PhaseIndex: &idx, PhaseKey: before.Phases[0].AuthorityKey(),
	}}

	widened, probs := FreezeFrom([]AuthoredPhase{{
		ID: "p", Label: "Phase", Why: "w",
		Permissions: []AuthoredPermission{
			{Handle: "perm:read:tracker_issue", Why: "w"},
			{Handle: "perm:write:tracker_issue", Why: "and now writes too"},
		},
		Slots: []AuthoredSlot{{Type: "tracker_issue", ID: "foo/bar", Why: "w"}},
	}}, demoSurface(t), []string{"tracker_issue"})
	require.Empty(t, probs)

	st, err := Fold(widened, records)
	require.NoError(t, err)
	assert.False(t, st.PhaseApproved(0),
		"a phase that gained a permission is not the phase that was approved")
}

// MIGRATION. plan_gate_audit is append-only, so records written before this
// change carry no PhaseKey. They must keep folding exactly as they did, or a
// session upgraded MID-RUN silently loses every approval it already has and
// starts re-asking for work the user already cleared.
func TestFold_recordsWithoutAPhaseKeyStillFoldByPlanAndIndex(t *testing.T) {
	plan := keyPlan(t, "perm:read:tracker_issue", "foo/bar")
	idx := int32(0)

	legacy := []plangateaudit.Content{{
		Event: plangateaudit.EventPhaseApproved,
		// No PhaseKey — as written by every binary before this slice.
		PlanDigest: plan.Digest(), PhaseIndex: &idx,
	}}

	st, err := Fold(plan, legacy)
	require.NoError(t, err)
	assert.True(t, st.PhaseApproved(0),
		"a pre-existing approval for THIS plan must still count")
}
