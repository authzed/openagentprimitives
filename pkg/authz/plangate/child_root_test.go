package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// A delegated child inherits ITS portion of the parent's plan — the phase the
// parent was in when it delegated — as its own approved ceiling. It must NOT
// inherit a later phase's ceiling: a debug child seeing the fix phase's write
// reach is the same info leak the "its portion" rule exists to prevent.
func TestDeriveForChild_CeilingIsInheritedPhaseOnly(t *testing.T) {
	readH := handle(t, "read", "ledger")
	writeH := handle(t, "write", "ledger")
	plan := Plan{Phases: []Phase{
		{Permissions: []permsurface.Handle{readH}},
		{Permissions: []permsurface.Handle{writeH}},
	}}

	// The parent delegated from phase 0 (the read phase).
	root, err := DeriveForChild(ChildRootInput{
		Parent:        "demo/parent",
		Plan:          plan,
		VisiblePhases: []int{0},
	})
	require.NoError(t, err)

	assert.Equal(t, plangateaudit.EventPlanApproved, root.Event)
	assert.Contains(t, root.Provenance, "delegated from demo/parent")
	assert.Equal(t, plan.Digest(), root.PlanDigest)
	assert.Contains(t, root.Ceiling, readH.String(), "the inherited phase's reach is in the child's ceiling")
	assert.NotContains(t, root.Ceiling, writeH.String(),
		"a phase the child did not inherit must not confer its reach")
}

// A child inheriting no visible phase (or a takeover-shaped empty projection)
// starts with an empty ceiling — it holds nothing until it asks.
func TestDeriveForChild_NoVisiblePhasesIsEmptyCeiling(t *testing.T) {
	plan := Plan{Phases: []Phase{{Permissions: []permsurface.Handle{handle(t, "read", "ledger")}}}}
	root, err := DeriveForChild(ChildRootInput{Parent: "demo/parent", Plan: plan, VisiblePhases: nil})
	require.NoError(t, err)
	assert.Empty(t, root.Ceiling, "a child given no phase holds nothing until it requests authority")
	assert.Contains(t, root.Provenance, "delegated from demo/parent")
}

// Denials travel: a handle a human refused on the parent's plan is carried into
// the child's root, or delegation launders it — deny, delegate, and the child
// no longer knows a human said no.
func TestDeriveForChild_CarriesParentDenials(t *testing.T) {
	readH := handle(t, "read", "ledger")
	deniedH := handle(t, "push", "extrepo_repo")
	plan := Plan{Phases: []Phase{{Permissions: []permsurface.Handle{readH}}}}

	root, err := DeriveForChild(ChildRootInput{
		Parent:        "demo/parent",
		Plan:          plan,
		VisiblePhases: []int{0},
		Denied:        [][]permsurface.Handle{{deniedH}},
	})
	require.NoError(t, err)
	assert.Contains(t, root.DeniedCeiling, deniedH.String(), "a parent's denial must reach the child")
}

// The load-bearing test: a child's inherited ceiling must survive the round trip
// through PlanFromRecords + Fold + ActiveCeiling, which is exactly what the
// child runner does. If Fold lands the child at a phase whose reconstructed
// ceiling is empty, the root is inert and the child holds nothing.
func TestDeriveForChild_FoldsToInheritedCeiling(t *testing.T) {
	readH := handle(t, "read", "ledger")
	writeH := handle(t, "write", "ledger")
	// Parent's plan: phase 0 read, phase 1 write. Parent delegated from phase 1.
	plan := Plan{Phases: []Phase{
		{Permissions: []permsurface.Handle{readH}},
		{Permissions: []permsurface.Handle{writeH}},
	}}
	root, err := DeriveForChild(ChildRootInput{
		Parent:        "demo/parent",
		Plan:          plan,
		VisiblePhases: []int{1}, // parent was in the write phase
	})
	require.NoError(t, err)

	// Reconstruct + fold exactly as the child runner does.
	childPlan, ok := PlanFromRecords([]plangateaudit.Content{root})
	require.True(t, ok, "the child's log must reconstruct a plan")
	st, err := Fold(childPlan, []plangateaudit.Content{root})
	require.NoError(t, err)

	ceiling, err := st.ActiveCeiling()
	require.NoError(t, err)
	assert.Contains(t, ceiling, writeH, "the child must actually HOLD the inherited phase's reach after folding")
	assert.NotContains(t, ceiling, readH, "a phase the child did not inherit must not appear")
}

// A read-only inherited phase auto-clears for the child exactly as the same
// phase auto-clears when an agent declares it via update_plan (tier-0). Without
// the clearance the child's own gate would fold the inherited phase as UNAPPROVED
// and raise a plan_phase — which a delegated child can neither answer (it is
// headless) nor self-clear (select_phase is withheld from children), so the
// inherited read ceiling would be unusable. This is the parity-restoring half of
// DeriveForChild: the CALLER decides the parent auto-cleared the phase, this
// builds the record that carries that clearance into the child's own log.
func TestDeriveChildPhaseClearance_MakesTheInheritedPhaseApproved(t *testing.T) {
	readH := handle(t, "list", "widget")
	plan := Plan{Phases: []Phase{{Permissions: []permsurface.Handle{readH}}}}
	root, err := DeriveForChild(ChildRootInput{Parent: "demo/parent", Plan: plan, VisiblePhases: []int{0}})
	require.NoError(t, err)

	clearance, ok := DeriveChildPhaseClearance(root)
	require.True(t, ok, "a root carrying an inherited phase must yield a clearance record")
	assert.Equal(t, plangateaudit.EventPhaseApproved, clearance.Event)
	assert.NotEmpty(t, clearance.PhaseKey, "the clearance must be key-bearing so it folds regardless of digest")

	// Fold exactly as the child runner does — root PLUS the clearance.
	childPlan, pok := PlanFromRecords([]plangateaudit.Content{root})
	require.True(t, pok)
	st, err := Fold(childPlan, []plangateaudit.Content{root, clearance})
	require.NoError(t, err)
	assert.True(t, st.PhaseApproved(0),
		"the inherited read-only phase must fold to APPROVED, so the child runs its ceiling without a human")

	// Ceiling still governs: the clearance clears the PHASE, it does not widen it.
	ceiling, cerr := st.ActiveCeiling()
	require.NoError(t, cerr)
	assert.Contains(t, ceiling, readH, "clearing the phase must not drop its reach")
}

// Without the clearance, the inherited phase folds UNAPPROVED — the state the
// bug produced, pinned so a regression that stops emitting the clearance is
// caught here rather than only in the e2e bundle.
func TestDeriveForChild_WithoutClearanceThePhaseIsUnapproved(t *testing.T) {
	readH := handle(t, "list", "widget")
	plan := Plan{Phases: []Phase{{Permissions: []permsurface.Handle{readH}}}}
	root, err := DeriveForChild(ChildRootInput{Parent: "demo/parent", Plan: plan, VisiblePhases: []int{0}})
	require.NoError(t, err)

	childPlan, pok := PlanFromRecords([]plangateaudit.Content{root})
	require.True(t, pok)
	st, err := Fold(childPlan, []plangateaudit.Content{root})
	require.NoError(t, err)
	assert.False(t, st.PhaseApproved(0),
		"a bare inherited root leaves the phase unapproved; the clearance is what makes it runnable")
}
