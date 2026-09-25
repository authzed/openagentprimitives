package plangate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

func hs(t *testing.T, names ...string) []permsurface.Handle {
	t.Helper()
	out := make([]permsurface.Handle, 0, len(names))
	for _, n := range names {
		h, err := permsurface.ParseHandle(n)
		require.NoError(t, err, "handle %q", n)
		out = append(out, h)
	}
	return out
}

func planWith(phases ...[]permsurface.Handle) plangate.Plan {
	p := plangate.Plan{}
	for i, perms := range phases {
		p.Phases = append(p.Phases, plangate.Phase{
			Label:       string(rune('A' + i)),
			Permissions: perms,
		})
	}
	return p
}

// The case the whole slice turns on: a re-plan that drops reach must not ask a
// human anything. Approval is invalidated structurally — the plan digest covers
// the permissions, so ANY edit re-keys every phase — and without a delta the
// gate would re-ask about a proposal that is strictly smaller than the one
// already approved.
func TestDiffPlans_NarrowingAsksNothing(t *testing.T) {
	approved := planWith(hs(t, "perm:read:doc", "perm:write:doc"))
	narrowed := planWith(hs(t, "perm:read:doc"))

	d := plangate.DiffPlans(approved, narrowed)

	assert.False(t, d.Widens(), "dropping reach cannot require consent")
	assert.Empty(t, d.AddedHandles())
	require.Len(t, d.Phases, 1, "the phase still CHANGED, so the card can narrate it")
	assert.Equal(t, hs(t, "perm:write:doc"), d.Phases[0].Removed)
}

// An untouched phase is not a question. A plan that widens phase 1 must not
// re-ask about phase 0, which nobody edited.
func TestDiffPlans_OnlyTouchedPhasesAppear(t *testing.T) {
	approved := planWith(hs(t, "perm:read:doc"), hs(t, "perm:read:issue"))
	widened := planWith(hs(t, "perm:read:doc"), hs(t, "perm:read:issue", "perm:write:issue"))

	d := plangate.DiffPlans(approved, widened)

	require.Len(t, d.Phases, 1, "phase 0 is untouched and must not appear")
	assert.Equal(t, 1, d.Phases[0].Index)
	assert.Equal(t, hs(t, "perm:write:issue"), d.Phases[0].Added)
}

// "One card, not N": several additions across several phases are one delta, and
// the handle set is deduped so a handle added to two phases is asked about once.
func TestDiffPlans_ManyAdditionsAcrossPhasesAreOneDelta(t *testing.T) {
	approved := planWith(hs(t, "perm:read:doc"), hs(t))
	widened := planWith(
		hs(t, "perm:read:doc", "perm:write:doc", "tool:apply_workspace"),
		hs(t, "perm:write:doc"),
	)

	d := plangate.DiffPlans(approved, widened)

	assert.True(t, d.Widens())
	require.Len(t, d.Phases, 2)
	assert.Equal(t, hs(t, "perm:write:doc", "tool:apply_workspace"),
		d.AddedHandles(), "deduped across phases and sorted")
}

// A phase the approved plan never had is entirely new reach — every handle in
// it is an addition, not an inheritance from whatever sat at that index before.
func TestDiffPlans_AppendedPhaseIsAllNew(t *testing.T) {
	approved := planWith(hs(t, "perm:read:doc"))
	extended := planWith(hs(t, "perm:read:doc"), hs(t, "perm:write:issue"))

	d := plangate.DiffPlans(approved, extended)

	require.Len(t, d.Phases, 1)
	assert.Equal(t, 1, d.Phases[0].Index)
	assert.Equal(t, hs(t, "perm:write:issue"), d.Phases[0].Added)
}

// Phases match BY INDEX, never by label. Matching on the agent's chosen name
// would let a rename inherit a different phase's approval — the substitution
// PhaseRef{digest,index} exists to prevent, and it must not reappear here.
func TestDiffPlans_RenamingAPhaseCannotInheritAnotherApproval(t *testing.T) {
	approved := plangate.Plan{Phases: []plangate.Phase{
		{Label: "recon", Permissions: hs(t, "perm:read:doc")},
		{Label: "write", Permissions: hs(t, "perm:write:doc")},
	}}
	// The agent swaps the labels, hoping the write ceiling rides in under the
	// name the approver cleared for reading.
	renamed := plangate.Plan{Phases: []plangate.Phase{
		{Label: "write", Permissions: hs(t, "perm:write:doc")},
		{Label: "recon", Permissions: hs(t, "perm:read:doc")},
	}}

	d := plangate.DiffPlans(approved, renamed)

	assert.True(t, d.Widens(), "position decides, so both phases changed")
	assert.Equal(t, hs(t, "perm:read:doc", "perm:write:doc"), d.AddedHandles())
}

// A phase can widen without adding a single handle. These two are the cases a
// permissions-only diff would wave through, and they are why Widens() covers
// every dimension the plan digest covers.
func TestDiffPlans_WideningWithoutANewHandle(t *testing.T) {
	base := plangate.Plan{Phases: []plangate.Phase{{
		Label:       "work",
		Permissions: hs(t, "perm:write:doc"),
		Max:         plangate.MaxSpec{Count: 1},
		Requires:    []plangate.RequiresEdge{{Phase: 0, Why: "recon first"}},
	}}}

	t.Run("raising the entry budget: same ceiling, exercised more times", func(t *testing.T) {
		raised := base
		raised.Phases = []plangate.Phase{base.Phases[0]}
		raised.Phases[0].Max = plangate.MaxSpec{Count: 5}

		d := plangate.DiffPlans(base, raised)

		require.Len(t, d.Phases, 1)
		assert.Empty(t, d.Phases[0].Added, "no handle was added")
		assert.True(t, d.Widens(), "5 runs of a ceiling approved for 1 is more authority")
		assert.Equal(t, 1, d.Phases[0].MaxWas)
		assert.Equal(t, 5, d.Phases[0].MaxNow)
	})

	t.Run("dropping a prerequisite: reachable at a point nobody agreed to", func(t *testing.T) {
		unGated := base
		unGated.Phases = []plangate.Phase{base.Phases[0]}
		unGated.Phases[0].Requires = nil

		d := plangate.DiffPlans(base, unGated)

		require.Len(t, d.Phases, 1)
		assert.Empty(t, d.Phases[0].Added)
		assert.True(t, d.Widens(), "removing an ordering constraint grants authority")
		assert.Equal(t, []int{0}, d.Phases[0].RequiresDropped)
	})

	t.Run("lowering the budget is a narrowing", func(t *testing.T) {
		lowered := base
		lowered.Phases = []plangate.Phase{base.Phases[0]}
		lowered.Phases[0].Max = plangate.MaxSpec{Count: 1}
		lowered.Phases[0].Permissions = nil

		d := plangate.DiffPlans(base, lowered)

		assert.False(t, d.Widens(), "dropping the ceiling entirely asks for nothing")
	})
}

// Re-wording an agent's justification is not a change in authority: the digest
// deliberately excludes `why`, and the delta must agree or every re-narration
// would cost a human an approval.
func TestDiffPlans_RewordingAJustificationIsNotADelta(t *testing.T) {
	before := plangate.Plan{Phases: []plangate.Phase{{
		Permissions: hs(t, "perm:read:doc"),
		Max:         plangate.MaxSpec{Count: 2, Why: "two repos"},
		Requires:    []plangate.RequiresEdge{{Phase: 0, Why: "recon first"}},
	}}}
	reworded := plangate.Plan{Phases: []plangate.Phase{{
		Permissions: hs(t, "perm:read:doc"),
		Max:         plangate.MaxSpec{Count: 2, Why: "there are two repositories to scan"},
		Requires:    []plangate.RequiresEdge{{Phase: 0, Why: "reconnaissance has to run first"}},
	}}}

	d := plangate.DiffPlans(before, reworded)

	assert.False(t, d.Widens())
	assert.Empty(t, d.Phases, "narration changed; authority did not")
}

// Reordering aside, an identical re-plan asks for nothing.
func TestDiffPlans_IdenticalPlanHasNoDelta(t *testing.T) {
	p := planWith(hs(t, "perm:read:doc", "perm:write:doc"), hs(t, "tool:apply_workspace"))

	d := plangate.DiffPlans(p, p)

	assert.False(t, d.Widens())
	assert.Empty(t, d.Phases, "nothing changed, so there is nothing to narrate")
}
