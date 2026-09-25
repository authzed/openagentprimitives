package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

func phaseTouching(t *testing.T, label string, slots []Slot, handles ...permsurface.Handle) Phase {
	t.Helper()
	return Phase{Label: label, Why: "because", Permissions: handles, Slots: slots}
}

// Telling the agent to name its resource did not work. The system prompt says
// "Name the RESOURCE, not just the permission", the recorded prompt from a live
// session contains that sentence verbatim, and the agent declared zero slots on
// all three phases anyway — so every card could name only a category and the
// user approved "some repository".
//
// Prose is a suggestion. This is the requirePlan shape instead, which did work:
// a precondition the agent must satisfy before its plan is accepted.
func TestMissingSlotDeclarations_aPhaseTouchingADeclaredTypeMustNameIt(t *testing.T) {
	p := Plan{Phases: []Phase{
		phaseTouching(t, "Push branch and open PR", nil, handle(t, "push", "git_repo")),
	}}

	missing := MissingSlotDeclarations(p, []string{"git_repo"})

	require.Len(t, missing, 1)
	assert.Equal(t, 0, missing[0].PhaseIndex)
	assert.Equal(t, "git_repo", missing[0].ResourceType)
	assert.Equal(t, "Push branch and open PR", missing[0].PhaseLabel,
		"the message has to name the phase, or the agent cannot tell which one to fix")
}

// The escape hatch, and the case most worth protecting. An agent that CANNOT
// name its target must have a legal way to say so — otherwise the only way to
// satisfy the precondition is to invent an id, and a slot id is AUTHORITY: it
// enters the digest and bounds the grant. Forcing a guess would be strictly
// worse than the silence this replaces.
func TestMissingSlotDeclarations_anExplicitlyDeferredTargetSatisfiesIt(t *testing.T) {
	p := Plan{Phases: []Phase{
		phaseTouching(t, "Find the repo", []Slot{{Type: "git_repo"}}, handle(t, "fetch", "git_repo")),
	}}

	assert.Empty(t, MissingSlotDeclarations(p, []string{"git_repo"}),
		"declaring the type without an id is the honest 'I do not know yet' and must be accepted")
}

func TestMissingSlotDeclarations_aNamedTargetSatisfiesIt(t *testing.T) {
	p := Plan{Phases: []Phase{
		phaseTouching(t, "Clone", []Slot{{Type: "git_repo", ID: "https://github.com/acme/app"}},
			handle(t, "fetch", "git_repo")),
	}}

	assert.Empty(t, MissingSlotDeclarations(p, []string{"git_repo"}))
}

// Only types the CLASS declared participate. A phase touching a resource type
// the class never opted into has no slot to declare — demanding one would make
// every class that has not adopted slots unable to plan at all.
func TestMissingSlotDeclarations_anUndeclaredTypeIsNotRequired(t *testing.T) {
	p := Plan{Phases: []Phase{
		phaseTouching(t, "Read issues", nil, handle(t, "read", "tracker_issue")),
	}}

	assert.Empty(t, MissingSlotDeclarations(p, []string{"git_repo"}),
		"the class declares no tracker_issue slot, so there is nothing to name")
	assert.Empty(t, MissingSlotDeclarations(p, nil),
		"a class with no slots at all keeps planning exactly as before")
}

// A tool handle names no SpiceDB resource, so it can never require a slot.
func TestMissingSlotDeclarations_aToolHandleRequiresNothing(t *testing.T) {
	h, err := permsurface.NewToolHandle("apply_workspace")
	require.NoError(t, err)

	p := Plan{Phases: []Phase{phaseTouching(t, "Apply", nil, h)}}

	assert.Empty(t, MissingSlotDeclarations(p, []string{"git_repo"}))
}

// Reported per (phase, type): one phase can touch two declared types, and the
// agent has to fix both. Collapsing them would send it round the loop twice.
func TestMissingSlotDeclarations_reportsEveryPhaseAndType(t *testing.T) {
	p := Plan{Phases: []Phase{
		phaseTouching(t, "Recon", nil, handle(t, "read", "git_repo")),
		phaseTouching(t, "Ship", nil, handle(t, "push", "git_repo"), handle(t, "write", "github_repo")),
	}}

	missing := MissingSlotDeclarations(p, []string{"git_repo", "github_repo"})

	require.Len(t, missing, 3)
	assert.Equal(t, 0, missing[0].PhaseIndex)
	assert.Equal(t, 1, missing[1].PhaseIndex)
	assert.Equal(t, 1, missing[2].PhaseIndex)
	// Stable order, because this text is rendered into a refusal the agent
	// reads; an order that moves between calls reads as a different problem.
	assert.Equal(t, "git_repo", missing[1].ResourceType)
	assert.Equal(t, "github_repo", missing[2].ResourceType)
}
