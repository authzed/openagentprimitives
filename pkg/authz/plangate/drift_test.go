package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// gateCall is what the gate records for one governed call.
func gateCall(p Plan, phase int, handle string) plangateaudit.Content {
	idx := int32(phase)
	return plangateaudit.Content{
		Event:      plangateaudit.EventGateAllowed,
		PlanDigest: p.Digest(),
		PhaseIndex: &idx,
		Handle:     handle,
		Outcome:    plangateaudit.OutcomeAllow,
	}
}

func twoPhasePlan(t *testing.T) Plan {
	t.Helper()
	return Plan{Phases: []Phase{
		{Label: "Recon", Permissions: hsIn(t, "perm:read:doc", "perm:list:doc"), Max: MaxSpec{Count: 1}},
		{Label: "Act", Permissions: hsIn(t, "perm:write:doc"), Max: MaxSpec{Count: 1}},
	}}
}

// The ratchet's input. "Declared but never exercised" is the concrete form of
// over-declaration, and it is only actionable PER PHASE: a plan-wide number
// cannot tell an operator which phase to narrow, and the ratchet drops handles
// from the phase that did not use them.
func TestPhaseDrift_reportsDeclaredButUnexercisedPerPhase(t *testing.T) {
	p := twoPhasePlan(t)
	records := []plangateaudit.Content{
		gateCall(p, 0, "perm:read:doc"), // phase 0 used one of its two
		gateCall(p, 1, "perm:write:doc"),
	}

	got := PhaseDrift(p, records)
	require.Len(t, got, 2)

	assert.Equal(t, []string{"perm:list:doc"}, got[0].DeclaredNeverExercised,
		"phase 0 asked for list and never used it")
	assert.Empty(t, got[1].DeclaredNeverExercised, "phase 1 used everything it declared")
}

// Usage is attributed to the phase that was ACTIVE for the call, not to whichever
// phase happens to declare the handle. Two phases may declare the same handle,
// and crediting the wrong one would report a phase as tight when it was the
// other that exercised its reach.
func TestPhaseDrift_attributesUsageToTheActivePhaseNotTheDeclaringOne(t *testing.T) {
	p := Plan{Phases: []Phase{
		{Label: "A", Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 1}},
		{Label: "B", Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 1}},
	}}
	// Only phase 1 ever ran.
	records := []plangateaudit.Content{gateCall(p, 1, "perm:read:doc")}

	got := PhaseDrift(p, records)
	require.Len(t, got, 2)

	assert.Equal(t, []string{"perm:read:doc"}, got[0].DeclaredNeverExercised,
		"phase 0 never ran; its declaration is pure over-declaration")
	assert.Empty(t, got[1].DeclaredNeverExercised)
}

// A phase that never ran at all is the strongest over-declaration signal there
// is, and it must be distinguishable from one that ran and used everything —
// both have an empty exercised set, and only one of them is a problem.
func TestPhaseDrift_marksAPhaseThatNeverRan(t *testing.T) {
	p := twoPhasePlan(t)
	records := []plangateaudit.Content{gateCall(p, 0, "perm:read:doc")}

	got := PhaseDrift(p, records)
	require.Len(t, got, 2)

	assert.True(t, got[0].Entered, "phase 0 governed a call")
	assert.False(t, got[1].Entered, "phase 1 never governed one")
}

// Records belonging to another plan are another ceiling's evidence. Blending
// them would credit this plan for reach it never declared, which is the same
// reason Metrics filters by digest.
func TestPhaseDrift_ignoresAnotherPlansRecords(t *testing.T) {
	p := twoPhasePlan(t)
	other := Plan{Phases: []Phase{{Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 9}}}}
	require.NotEqual(t, p.Digest(), other.Digest())

	got := PhaseDrift(p, []plangateaudit.Content{gateCall(other, 0, "perm:read:doc")})
	require.Len(t, got, 2)

	assert.False(t, got[0].Entered)
	assert.Equal(t, []string{"perm:list:doc", "perm:read:doc"}, got[0].DeclaredNeverExercised)
}

// hsIn builds handles from their wire spelling for the internal-package tests.
func hsIn(t *testing.T, names ...string) []permsurface.Handle {
	t.Helper()
	out := make([]permsurface.Handle, 0, len(names))
	for _, n := range names {
		h, err := permsurface.ParseHandle(n)
		require.NoError(t, err, "handle %q", n)
		out = append(out, h)
	}
	return out
}
