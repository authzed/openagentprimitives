package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

// MaxSpec.Count — the per-phase ENTRY budget — was recorded, digested, carded
// and advertised to the model, and compared to nothing.
//
// State.Spent is the ledger, and its only readers asked `> 0` ("was this phase
// entered at all"). There was no analogue of BudgetExhausted, which does
// enforce Budget.Calls. Two symptoms of the same gap corroborate it:
// plangate.KindExtraEntry and Request.BudgetSpent both exist and are consumed
// by Classify, and neither has any producer in the tree — nothing ever asks for
// an extra entry because nothing ever refuses one.
//
// What that made false: the approver's card and record state maxCount, MaxSpec's
// own doc says "an approver can see 'this phase runs once'", and select_phase's
// LLM-visible description tells the model "A phase may normally be entered once
// — re-entering one you have already used spends another of its allowed
// entries, and running out means asking the user." An agent approved for a
// phase declared max.count: 1 re-entered it without bound. A phase declaring
// max: 1 and NO call budget — the common shape, since Calls: 0 means unbounded
// — was unbounded in both dimensions while the card said "once".
func TestSelectPhase_RefusesAPhaseThatHasSpentItsEntries(t *testing.T) {
	entries := map[int]int{}
	var recorded int
	tl := meta.NewSelectPhase(meta.SelectPhaseConfig{
		ActivePlan:   func(context.Context) (plangate.Plan, bool) { return twoPhaseFrozen(t), true },
		PhaseEntries: func(context.Context) map[int]int { return entries },
		Record: func(_ context.Context, idx int) error {
			recorded++
			entries[idx]++
			return nil
		},
	})
	run := func() tool.Result {
		res, err := tl.Execute(context.Background(), json.RawMessage(`{"index":1}`), nil)
		require.NoError(t, err)
		return res
	}

	first := run()
	require.False(t, first.IsError, "the first entry is what max.count: 1 allows: %s", first.Content)
	require.Equal(t, 1, recorded)

	second := run()
	assert.True(t, second.IsError,
		"a phase declared max.count: 1 must not be entered twice — the approver's card said it runs once")
	assert.Contains(t, second.Content, "select_phase:")
	assert.Equal(t, 1, recorded,
		"and the refused transition must not reach the log, or the fold would move the agent into it anyway")
}

// A phase declaring no max is unbounded, and must stay so: Count is zero when
// the author declared nothing, and turning that into "zero entries" would wedge
// every plan that never mentioned it.
func TestSelectPhase_AnUndeclaredMaxIsUnbounded(t *testing.T) {
	plan := twoPhaseFrozen(t)
	plan.Phases[1].Max = plangate.MaxSpec{} // declared nothing

	entries := map[int]int{1: 7}
	tl := meta.NewSelectPhase(meta.SelectPhaseConfig{
		ActivePlan:   func(context.Context) (plangate.Plan, bool) { return plan, true },
		PhaseEntries: func(context.Context) map[int]int { return entries },
		Record:       func(context.Context, int) error { return nil },
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"index":1}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError, "an undeclared max means unbounded, not zero: %s", res.Content)
}

// A wiring gap must not wedge every session: nil PhaseEntries means the runtime
// cannot establish the count, and the transition proceeds — the same stance
// PhaseCompletion takes for the same reason.
func TestSelectPhase_ANilEntryCounterDoesNotBlock(t *testing.T) {
	tl := meta.NewSelectPhase(meta.SelectPhaseConfig{
		ActivePlan: func(context.Context) (plangate.Plan, bool) { return twoPhaseFrozen(t), true },
		Record:     func(context.Context, int) error { return nil },
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"index":1}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError, "%s", res.Content)
}
