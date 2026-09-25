package plangate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// seedParentPlan writes one faithful EventPlanApproved record per phase into the
// parent's scope, exactly as FreezeAndRecordPhases would, tagged with the given
// tier. Faithful matters: WriteChildRoot rebuilds the parent plan via
// PlanFromRecords and reads the tier off the record for the ACTIVE phase, so the
// records must reconstruct a plan whose digest equals what it was stored under.
func seedParentPlan(t *testing.T, mem memory.Memory, parent string, plan Plan, tier string) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	pscope := memory.Scope{Kind: "session", ID: parent}
	for i := range plan.Phases {
		rec := PhaseAuthorityRecord(plan, i, nil)
		rec.Event = plangateaudit.EventPlanApproved
		rec.PlanDigest = plan.Digest()
		rec.Tier = tier
		require.NoError(t, plangateaudit.Record(ctx, mem, pscope, rec))
	}
}

// childState reads the child's plan-gate log back and folds it exactly as the
// child runner does.
func childState(t *testing.T, mem memory.Memory, child string) State {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	recs, err := plangateaudit.List(ctx, mem, memory.Scope{Kind: "session", ID: child})
	require.NoError(t, err)
	plan, ok := PlanFromRecords(recs)
	require.True(t, ok, "the child log must reconstruct a plan")
	st, err := Fold(plan, recs)
	require.NoError(t, err)
	return st
}

func childRecords(t *testing.T, mem memory.Memory, child string) []plangateaudit.Content {
	t.Helper()
	recs, err := plangateaudit.List(memory.WithSystemApproval(context.Background(), "test"), mem,
		memory.Scope{Kind: "session", ID: child})
	require.NoError(t, err)
	return recs
}

func countEvents(recs []plangateaudit.Content, event string) int {
	n := 0
	for _, r := range recs {
		if r.Event == event {
			n++
		}
	}
	return n
}

// A read-only (tier-0) parent phase makes the child's inherited phase run WITHOUT
// a human: WriteChildRoot writes both the root AND the tier-0 clearance, and the
// child's own fold reports the phase APPROVED. This is the parity fix — the same
// phase auto-clears when declared, so it must auto-clear when inherited.
func TestWriteChildRoot_ReadonlyPhaseAutoClearsForChild(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	readH := handle(t, "list", "widget")
	seedParentPlan(t, mem, "demo/parent",
		Plan{Phases: []Phase{{Permissions: []permsurface.Handle{readH}}}}, "0")

	require.NoError(t, WriteChildRoot(context.Background(), mem, "demo/parent", "demo/child"))

	st := childState(t, mem, "demo/child")
	assert.True(t, st.PhaseApproved(0),
		"an inherited read-only phase must fold APPROVED so the child runs it with no human")
	ceiling, err := st.ActiveCeiling()
	require.NoError(t, err)
	assert.Contains(t, ceiling, readH, "clearing the phase must not drop its reach")
}

// A phase the parent needed a HUMAN to clear (tier 1: it writes) is NOT laundered
// into an automatic child clearance. The child inherits the ceiling but its phase
// folds UNAPPROVED — it re-asks, routed to the parent's approvers.
func TestWriteChildRoot_NonTier0PhaseDoesNotAutoClear(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	writeH := handle(t, "write", "ledger")
	seedParentPlan(t, mem, "demo/parent",
		Plan{Phases: []Phase{{Permissions: []permsurface.Handle{writeH}}}}, "1")

	require.NoError(t, WriteChildRoot(context.Background(), mem, "demo/parent", "demo/child"))

	st := childState(t, mem, "demo/child")
	assert.False(t, st.PhaseApproved(0),
		"a phase the parent needed a human to clear must not auto-clear for the child")
	// The ceiling still travels — the child holds the reach, it just cannot run it
	// until cleared.
	ceiling, err := st.ActiveCeiling()
	require.NoError(t, err)
	assert.Contains(t, ceiling, writeH)

	recs := childRecords(t, mem, "demo/child")
	assert.Equal(t, 0, countEvents(recs, plangateaudit.EventPhaseApproved),
		"no tier-0 clearance may be written for a non-tier-0 phase")
}

// Running twice stacks neither a second root nor a second clearance: the audit
// chain is append-only, and a duplicate would be noise (and, with a timestamp,
// an append-only conflict).
func TestWriteChildRoot_IsIdempotent(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	readH := handle(t, "list", "widget")
	seedParentPlan(t, mem, "demo/parent",
		Plan{Phases: []Phase{{Permissions: []permsurface.Handle{readH}}}}, "0")

	require.NoError(t, WriteChildRoot(context.Background(), mem, "demo/parent", "demo/child"))
	require.NoError(t, WriteChildRoot(context.Background(), mem, "demo/parent", "demo/child"))

	recs := childRecords(t, mem, "demo/child")
	assert.Equal(t, 1, countEvents(recs, plangateaudit.EventPlanApproved), "exactly one root")
	assert.Equal(t, 1, countEvents(recs, plangateaudit.EventPhaseApproved), "exactly one clearance")
}

// A child left with ONLY its root — a partial first write, or one written before
// this parity fix existed — gains the missing clearance on the next reconcile,
// rather than being stranded needing a human to clear a phase it cannot clear.
func TestWriteChildRoot_BackfillsAMissingClearance(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	readH := handle(t, "list", "widget")
	parentPlan := Plan{Phases: []Phase{{Permissions: []permsurface.Handle{readH}}}}
	seedParentPlan(t, mem, "demo/parent", parentPlan, "0")

	// Simulate the partial state: the root landed, the clearance did not.
	ctx := memory.WithSystemApproval(context.Background(), "test")
	root, err := DeriveForChild(ChildRootInput{Parent: "demo/parent", Plan: parentPlan, VisiblePhases: []int{0}})
	require.NoError(t, err)
	require.NoError(t, plangateaudit.Record(ctx, mem, memory.Scope{Kind: "session", ID: "demo/child"}, root))
	require.False(t, childState(t, mem, "demo/child").PhaseApproved(0), "precondition: phase not yet cleared")

	require.NoError(t, WriteChildRoot(context.Background(), mem, "demo/parent", "demo/child"))

	st := childState(t, mem, "demo/child")
	assert.True(t, st.PhaseApproved(0), "the missing clearance must be backfilled")
	recs := childRecords(t, mem, "demo/child")
	assert.Equal(t, 1, countEvents(recs, plangateaudit.EventPlanApproved), "no duplicate root")
	assert.Equal(t, 1, countEvents(recs, plangateaudit.EventPhaseApproved), "exactly one clearance")
}
