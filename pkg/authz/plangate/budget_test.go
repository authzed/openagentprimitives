package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// A per-phase budget is AUTHORITY over time: the same ceiling exercised more
// often reaches more. Raising it is therefore a widening, exactly as raising the
// entry count is, and the digest has to cover it or an agent could grant itself
// a bigger budget under an unchanged digest and carry an old approval forward.
func TestDigest_coversThePhaseCallBudget(t *testing.T) {
	base := Plan{Phases: []Phase{{Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 5}}}}
	bigger := Plan{Phases: []Phase{{Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 50}}}}

	assert.NotEqual(t, base.Digest(), bigger.Digest())
}

// The budget's justification is narration like every other Why, and narration
// never re-keys a plan.
func TestDigest_theBudgetWhyIsNarration(t *testing.T) {
	a := Plan{Phases: []Phase{{Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 5, Why: "one reason"}}}}
	b := Plan{Phases: []Phase{{Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 5, Why: "quite another"}}}}

	assert.Equal(t, a.Digest(), b.Digest())
}

// The fifth widening dimension. A re-plan can widen without touching handles,
// entries, prerequisites or slots — by asking to spend longer inside the same
// ceiling, which is precisely the rabbit-holing the budget exists to bound.
func TestDiffPhase_raisingTheCallBudgetWidens(t *testing.T) {
	granted := Phase{Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 5}}
	asking := Phase{Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 40}}

	d, changed := DiffPhase(granted, asking, 0)
	require.True(t, changed)
	assert.True(t, d.Widens(), "spending longer inside a ceiling reaches more")
}

// Asking for a SMALLER budget is asking for less, so it cannot need consent.
func TestDiffPhase_loweringTheCallBudgetDoesNotWiden(t *testing.T) {
	granted := Phase{Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 40}}
	asking := Phase{Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 5}}

	d, changed := DiffPhase(granted, asking, 0)
	require.True(t, changed)
	assert.False(t, d.Widens())
}

// The fold counts what the phase actually spent, from the log — the same
// records the ceiling test already reads, so a budget needs no new state and
// cannot drift from what the gate observed.
func TestFold_countsGatedCallsPerPhase(t *testing.T) {
	p := Plan{Phases: []Phase{
		{Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 5}},
		{Permissions: hsIn(t, "perm:write:doc"), Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 5}},
	}}
	records := []plangateaudit.Content{
		gateCall(p, 0, "perm:read:doc"),
		gateCall(p, 0, "perm:read:doc"),
		gateCall(p, 1, "perm:write:doc"),
	}

	st, err := Fold(p, records)
	require.NoError(t, err)

	assert.Equal(t, 2, st.CallsSpent[0])
	assert.Equal(t, 1, st.CallsSpent[1])
}

// A call the gate does not govern — a meta or passthrough tool with no handle —
// spends nothing. Counting them would let respond_to_user and update_plan burn
// the budget an agent needs to do the actual work, and re-planning would be the
// very thing that exhausts the budget forcing the re-plan.
func TestFold_ungovernedCallsDoNotSpendTheBudget(t *testing.T) {
	p := Plan{Phases: []Phase{{Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 1}}}}
	noHandle := gateCall(p, 0, "")
	records := []plangateaudit.Content{noHandle, noHandle, gateCall(p, 0, "perm:read:doc")}

	st, err := Fold(p, records)
	require.NoError(t, err)

	assert.Equal(t, 1, st.CallsSpent[0])
}

// BudgetExhausted is the question the gate asks per call. Zero means the phase
// declared no budget of its own, and an undeclared budget must not read as "no
// calls allowed" — that would deny every phase that did not opt in.
func TestBudgetExhausted_zeroMeansUnbounded(t *testing.T) {
	p := Plan{Phases: []Phase{{Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 1}}}}
	st, err := Fold(p, []plangateaudit.Content{
		gateCall(p, 0, "perm:read:doc"), gateCall(p, 0, "perm:read:doc"),
	})
	require.NoError(t, err)

	assert.False(t, st.BudgetExhausted(0), "a phase that declared no budget is not out of one")
}

func TestBudgetExhausted_reportsAPhaseAtItsLimit(t *testing.T) {
	p := Plan{Phases: []Phase{
		{Permissions: hsIn(t, "perm:read:doc"), Max: MaxSpec{Count: 1}, Budget: BudgetSpec{Calls: 2}},
	}}
	call := gateCall(p, 0, "perm:read:doc")

	underBudget, err := Fold(p, []plangateaudit.Content{call})
	require.NoError(t, err)
	assert.False(t, underBudget.BudgetExhausted(0), "one of two spent")

	atBudget, err := Fold(p, []plangateaudit.Content{call, call})
	require.NoError(t, err)
	assert.True(t, atBudget.BudgetExhausted(0), "both spent; the next call needs a re-plan")
}

// The budget is in the digest, so a plan rebuilt from the log must carry it or
// it digests differently from the plan the records name — and the fold then
// discards every one of them as belonging to another plan.
//
// This is the SAME coupling the slot round-trip test pins, and it was broken
// again the moment a fifth dimension landed: adding to the digest without
// adding to the record is a silent break, because nothing fails until a reader
// tries to reconstruct. A bronze bundle caught it; this is the unit-level guard
// so the next dimension does not need a whole-session scenario to notice.
func TestPlanForDigest_roundTripsTheCallBudget(t *testing.T) {
	p := Plan{Phases: []Phase{{
		Permissions: hsIn(t, "perm:read:doc"),
		Max:         MaxSpec{Count: 1},
		Budget:      BudgetSpec{Calls: 7},
	}}}

	got, ok := PlanForDigest(planApprovedRecordsFor(p), p.Digest())
	require.True(t, ok)
	assert.Equal(t, p.Digest(), got.Digest(),
		"a rebuilt plan must digest identically or every fold discards its records")
	assert.Equal(t, 7, got.Phases[0].Budget.Calls)
}
