package plangate

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// A parent's plan-gate ceiling is projected into the child's audit scope
// unconditionally — but it is INERT DATA unless the child's own runner builds a
// plan gate, and that build was gated on the child CLASS's resolved mode. So a
// parent under an enforcing gate delegating to a class with the gate disabled
// handed its child MORE reach than the parent itself held, and neither backstop
// bounds it: the tool authz check is per-call against the child's own surface
// and can itself be permissive, and session scope is per-class. The repo says
// so in its own bundle fixture — "its child class is not enforcing, so the
// inherited ceiling is inert and nothing ever consults it to deny."
//
// So the ceiling carries its own enforcement: a session holding an inherited
// root runs the gate whatever its class says. The marker that says so is a
// FIELD on the derived root, not the Provenance sentence, which is prose for a
// human and one re-wording away from silently turning the control off.

func TestDeriveForChild_MarksTheRootAsDelegated(t *testing.T) {
	root, err := DeriveForChild(ChildRootInput{
		Parent:        "demo/parent",
		Plan:          Plan{Phases: []Phase{{Permissions: []permsurface.Handle{handle(t, "list", "widget")}}}},
		VisiblePhases: []int{0},
	})
	require.NoError(t, err)

	assert.Equal(t, "demo/parent", root.DelegatedFrom,
		"the root names its parent in a field, so enforcement does not hang off parsing prose")
}

// The question the runner asks at startup, over the child's own log.
func TestHasInheritedCeiling_TrueForADelegatedRoot(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	scope := memory.Scope{Kind: "session", ID: "demo/child"}

	root, err := DeriveForChild(ChildRootInput{
		Parent:        "demo/parent",
		Plan:          Plan{Phases: []Phase{{Permissions: []permsurface.Handle{handle(t, "list", "widget")}}}},
		VisiblePhases: []int{0},
	})
	require.NoError(t, err)
	require.NoError(t, plangateaudit.Record(ctx, mem, scope, root))

	got, err := HasInheritedCeiling(ctx, mem, scope)
	require.NoError(t, err)
	assert.True(t, got, "a child holding a projected ceiling must enforce it")
}

// A session that ran its OWN gate has plan_approved records too. They are not
// inherited, and treating them as such would flip the mode of any session whose
// class deliberately runs the gate in logging mode.
func TestHasInheritedCeiling_FalseForASessionsOwnApproval(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	scope := memory.Scope{Kind: "session", ID: "demo/own"}

	require.NoError(t, plangateaudit.Record(ctx, mem, scope, plangateaudit.Content{
		Event:      plangateaudit.EventPlanApproved,
		PlanDigest: "sha256:whatever",
	}))

	got, err := HasInheritedCeiling(ctx, mem, scope)
	require.NoError(t, err)
	assert.False(t, got, "a session's own approval is not an inherited ceiling")
}

// No records at all — a session that never ran a gate and inherited nothing. It
// must stay whatever its class said, or every ordinary session would be
// force-enforced with no plan and deny everything.
func TestHasInheritedCeiling_FalseForAnEmptyLog(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	got, err := HasInheritedCeiling(ctx, mem, memory.Scope{Kind: "session", ID: "demo/fresh"})
	require.NoError(t, err)
	assert.False(t, got)
}

// failingInheritMemory fails every read, standing in for a memory outage at
// runner startup.
type failingInheritMemory struct{ memory.Memory }

func (failingInheritMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, errors.New("memory: simulated outage")
}

// A read failure is NOT "no ceiling". Answering false on an error would make a
// memory blip silently disable the inherited gate — which is the exact failure
// this control exists to prevent — so the error travels and the caller decides.
func TestHasInheritedCeiling_ReadErrorIsReturned(t *testing.T) {
	_, err := HasInheritedCeiling(context.Background(), failingInheritMemory{}, memory.Scope{Kind: "session", ID: "demo/x"})
	require.Error(t, err, "an unreadable log must not read as 'inherited nothing'")
}

// EffectiveMode is the one place the rule lives, so both wiring sites — the
// runner binary and the in-process e2e harness, which builds its own Loop —
// give the same answer instead of one of them quietly omitting it.
func TestEffectiveMode(t *testing.T) {
	cases := []struct {
		name      string
		classMode string
		inherited bool
		want      string
	}{
		{"disabled class, inherited ceiling: enforcing", "disabled", true, ModeEnforcing},
		{"unset class, inherited ceiling: enforcing", "", true, ModeEnforcing},
		{"logging class, inherited ceiling: enforcing", "logging", true, ModeEnforcing},
		{"enforcing class, inherited ceiling: unchanged", ModeEnforcing, true, ModeEnforcing},
		{"disabled class, nothing inherited: unchanged", "disabled", false, "disabled"},
		{"logging class, nothing inherited: unchanged", "logging", false, "logging"},
		{"unset class, nothing inherited: unchanged", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, EffectiveMode(tc.classMode, tc.inherited))
		})
	}
}
