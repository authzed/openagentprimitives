package pipeline

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAHumanTurnRefillsTheBudget is the structural bound.
//
// Only a HUMAN turn refills. That is the whole control: past the budget the
// agent→agent loop cannot continue without a person. Refilling on any turn
// would make the budget decorative while every spending test still passed.
func TestAHumanTurnRefillsTheBudget(t *testing.T) {
	c := RefillOnHumanTurn(3)
	assert.Equal(t, 3, c.Remaining)

	spent, ok := SpendForAgentWake(c)
	assert.True(t, ok)
	assert.Equal(t, 2, spent.Remaining)

	// A human speaks again mid-budget: back to full, not to 3 more on top.
	assert.Equal(t, 3, RefillOnHumanTurn(3).Remaining,
		"a refill sets the budget, it does not accumulate — otherwise a chatty thread banks credit for a later loop")
}

// TestSpendingStopsAtZeroAndReportsIt.
//
// The bool is what the caller acts on: at zero it appends without waking. A
// counter allowed to go negative would read as a large deficit on the next
// comparison and could take several human turns to climb back to positive,
// silently extending the outage past the one turn intended.
func TestSpendingStopsAtZeroAndReportsIt(t *testing.T) {
	c := RefillOnHumanTurn(2)

	c, ok := SpendForAgentWake(c)
	assert.True(t, ok)
	c, ok = SpendForAgentWake(c)
	assert.True(t, ok)
	assert.Equal(t, 0, c.Remaining)

	after, ok := SpendForAgentWake(c)
	assert.False(t, ok, "at zero the caller must learn it may not wake, rather than being handed a negative budget")
	assert.Equal(t, 0, after.Remaining, "the counter must not go negative")
}

// TestAZeroBudgetMeansOffNotUnlimited pins the direction of the default.
//
// A class that declares nothing gets zero, and zero means agent→agent wakes are
// OFF. DenialStreakDeps.Threshold documents the same trap pointing the other
// way — "ZERO MEANS DISABLED — an unset threshold must never read as freeze
// everything" — and here the unsafe reading would be "no ceiling", which is
// precisely the loop this track exists to bound.
func TestAZeroBudgetMeansOffNotUnlimited(t *testing.T) {
	c := RefillOnHumanTurn(0)
	assert.Equal(t, 0, c.Remaining)

	_, ok := SpendForAgentWake(c)
	assert.False(t, ok,
		"an unset budget must disable agent→agent wakes, never grant unlimited ones")
}

// TestANegativeBudgetIsClampedToOff: a negative declared budget is a
// configuration mistake, and the safe reading of it is off rather than an
// enormous allowance produced by a sign error somewhere upstream.
func TestANegativeBudgetIsClampedToOff(t *testing.T) {
	c := RefillOnHumanTurn(-5)
	assert.Equal(t, 0, c.Remaining)
	_, ok := SpendForAgentWake(c)
	assert.False(t, ok)
}

// TestSeeingIsNeverBounded is the see/react split, and the assertion pair
// matters more than either half.
//
// Both facts are asserted in both cases. Checking only Wake would pass against
// an implementation that DROPPED the message when out of credit — and a
// dropped message is the failure that makes a thread look broken to the human
// watching it: the agent goes silent with no explanation and no record.
func TestSeeingIsNeverBounded(t *testing.T) {
	withCredit := DecideWake(true, 2, WakeCredit{Remaining: 1})
	assert.True(t, withCredit.Append)
	assert.True(t, withCredit.Wake)
	assert.Equal(t, 0, withCredit.Credit.Remaining)

	exhausted := DecideWake(true, 2, WakeCredit{Remaining: 0})
	assert.True(t, exhausted.Append,
		"an agent message ALWAYS appends: the session must see the conversation even when it will not answer")
	assert.False(t, exhausted.Wake)
}

// TestOnlyAHumanTurnRefills is the property that makes the bound structural.
//
// If an agent turn refilled, the budget would be decorative and every spending
// test above would still pass — which is exactly why this is asserted
// separately rather than inferred from them.
func TestOnlyAHumanTurnRefills(t *testing.T) {
	spent := DecideWake(true, 3, WakeCredit{Remaining: 1})
	assert.Equal(t, 0, spent.Credit.Remaining, "an agent turn spends and never refills")

	human := DecideWake(false, 3, WakeCredit{Remaining: 0})
	assert.True(t, human.Wake, "a person is never rate-limited by the agent budget")
	assert.Equal(t, 3, human.Credit.Remaining, "a human turn restores the budget")

	// And the loop resumes after the person speaks: this is a pause, not a
	// terminal state.
	resumed := DecideWake(true, 3, human.Credit)
	assert.True(t, resumed.Wake)
}

// TestAZeroBudgetStillLetsHumansThrough: with cross-agent wakes off, a person's
// message must behave exactly as it always did.
func TestAZeroBudgetStillLetsHumansThrough(t *testing.T) {
	d := DecideWake(false, 0, WakeCredit{})
	assert.True(t, d.Append)
	assert.True(t, d.Wake, "turning agent→agent wakes off must not mute the humans in the thread")
}
