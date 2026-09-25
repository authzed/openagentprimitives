package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

func keyHandles(t *testing.T, hs ...string) []permsurface.Handle {
	t.Helper()
	out := make([]permsurface.Handle, 0, len(hs))
	for _, s := range hs {
		h, err := permsurface.ParseHandle(s)
		require.NoError(t, err, "ParseHandle(%q)", s)
		out = append(out, h)
	}
	return out
}

// AuthorityKey is what lets a human's approval survive a re-plan: a key-bearing
// record is folded ahead of the foreign-plan skip precisely so a reshaped plan
// does not re-ask for reach the person already cleared. Its own doc says
// "everything that widens what the phase can do IS in here."
//
// Two dimensions were not. Plan.Digest hashes Max.Count and Requires, and
// PhaseDelta.Widens() counts both — Widens' doc names the hazard exactly: "Keep
// this in lockstep with Plan.Digest … A dimension in one and not the other is a
// hole in exactly one direction — and it is the direction nobody notices."
//
// The escape: a human approves a 2-phase plan where phase 1 declares
// `requires: [phase 0]`, so the review runs first. The agent calls update_plan
// with an identical phase 1 minus that edge. The plan digest changes, so
// approvedPhases misses — but AuthorityKey is byte-identical, approvedKeys
// HITS, and PhaseApproved returns true before CarriesOver (which would have
// caught it through droppedRequires) is ever consulted. select_phase then
// enforces prerequisites against the CURRENT plan, which no longer has the
// edge. The agent enters the push phase immediately, skipping the review the
// approver was told would come first, with zero human interaction.
func TestAuthorityKey_CoversEveryDimensionThatWidensThePhase(t *testing.T) {
	base := Phase{
		Permissions: keyHandles(t, "perm:write:tracker_issue"),
		Max:         MaxSpec{Count: 1},
		Requires:    []RequiresEdge{{Phase: 0}},
		Budget:      BudgetSpec{Calls: 5},
	}

	widened := []struct {
		name string
		with func(Phase) Phase
	}{
		{
			name: "raising max.count: the same ceiling entered more times reaches further",
			with: func(p Phase) Phase { p.Max = MaxSpec{Count: 9}; return p },
		},
		{
			name: "dropping a requires edge: a prerequisite the approver was shown no longer gates",
			with: func(p Phase) Phase { p.Requires = nil; return p },
		},
		{
			name: "changing which phase is required: a different prerequisite is a different promise",
			with: func(p Phase) Phase { p.Requires = []RequiresEdge{{Phase: 2}}; return p },
		},
	}
	for _, tc := range widened {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEqual(t, base.AuthorityKey(), tc.with(base).AuthorityKey(),
				"a phase that changed this must not inherit the earlier approval")
		})
	}
}

// The other half, and the reason the key exists at all: a phase that changed
// NOTHING about its authority must keep its approval across a re-plan. A
// re-worded justification is the case the key was introduced for — an agent may
// re-word freely, it may not re-word its way into new authority.
func TestAuthorityKey_IsStableAcrossThingsThatDoNotWiden(t *testing.T) {
	base := Phase{
		Permissions: keyHandles(t, "perm:write:tracker_issue"),
		Max:         MaxSpec{Count: 1},
		Requires:    []RequiresEdge{{Phase: 0}},
		Budget:      BudgetSpec{Calls: 5},
	}

	reworded := base
	reworded.Why = "a completely different explanation of the same work"
	reworded.Label = "Renamed"
	reworded.Max.Why = "and a different story about the entry count"

	assert.Equal(t, base.AuthorityKey(), reworded.AuthorityKey(),
		"narration is not authority; re-asking under a new story is how people are trained to click through")
}
