package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoopAddToolCost_AccumulatesPerTool(t *testing.T) {
	var l Loop
	// codebot's shape: one tool ("claude-oauth") invoked many times -> ONE
	// summed bucket; a second tool -> a distinct bucket, first-seen order.
	l.AddToolCost("claude-oauth", 5.43, true, false)
	l.AddToolCost("claude-oauth", 1.31, true, false)
	l.AddToolCost("aider", 2.10, true, false)

	got := l.usageByToolSnapshot()
	require.Len(t, got, 2, "two distinct tools -> two buckets in first-encountered order")

	assert.Equal(t, "claude-oauth", got[0].Tool)
	assert.True(t, got[0].CostReported)
	assert.Equal(t, int64(6_740_000), got[0].CostMicroUSD, "5.43+1.31 USD -> 6,740,000 micro-USD")

	assert.Equal(t, "aider", got[1].Tool)
	assert.Equal(t, int64(2_100_000), got[1].CostMicroUSD)
}

func TestLoopAddToolCost_NegativeCostIgnored(t *testing.T) {
	var l Loop
	l.AddToolCost("claude-oauth", -1.0, true, false)

	got := l.usageByToolSnapshot()
	require.Len(t, got, 1, "the tool is still recorded as seen")
	assert.False(t, got[0].CostReported, "negative cost -> treated as unreported")
	assert.Equal(t, int64(0), got[0].CostMicroUSD, "no negative amount is ever added")
	assert.True(t, got[0].Anomalous, "negative cost -> flagged as not making sense")
}

func TestLoopAddToolCost_ZeroCostUnbilled(t *testing.T) {
	// A run whose terminal event carried NO cost tally (costReported=false) adds
	// 0 and does NOT mark the bucket priced — an absent cost is never asserted as
	// a known $0 (the absent-vs-zero distinction Outcome.Unbilled documents).
	var l Loop
	l.AddToolCost("claude-oauth", 0, false, false)

	got := l.usageByToolSnapshot()
	require.Len(t, got, 1)
	assert.Equal(t, int64(0), got[0].CostMicroUSD)
	assert.False(t, got[0].CostReported, "absent cost -> bucket is not priced")
}

func TestLoopAddToolCost_ExplicitZeroReported(t *testing.T) {
	// An explicit total_cost_usd:0 IS a reported tally: amount 0, but priced —
	// distinct from an absent cost.
	var l Loop
	l.AddToolCost("claude-oauth", 0, true, false)

	got := l.usageByToolSnapshot()
	require.Len(t, got, 1)
	assert.Equal(t, int64(0), got[0].CostMicroUSD)
	assert.True(t, got[0].CostReported, "explicit $0 is a known price, not 'unreported'")
}

func TestLoopAddToolCost_SubscriptionEquivalentFlagged(t *testing.T) {
	// A subscription-equivalent toolkit's cost is recorded on the bucket and
	// flagged; the cost reporter / admind exclude it from spend on that flag.
	var l Loop
	l.AddToolCost("claude-oauth", 5.43, true, true)

	got := l.usageByToolSnapshot()
	require.Len(t, got, 1)
	assert.Equal(t, int64(5_430_000), got[0].CostMicroUSD, "amount is still recorded on the bucket")
	assert.True(t, got[0].SubscriptionEquivalent, "flagged so downstream excludes it from spend")
}

func TestLoopAddToolCost_SubscriptionFlagSurvivesNegativeReport(t *testing.T) {
	// The subscription flag is a property of the tool, recorded before the
	// negative-cost guard returns, so a negative-only report still carries it.
	var l Loop
	l.AddToolCost("claude-oauth", -1.0, true, true)

	got := l.usageByToolSnapshot()
	require.Len(t, got, 1)
	assert.True(t, got[0].SubscriptionEquivalent, "flag recorded even when the cost report is dropped")
	assert.True(t, got[0].Anomalous)
}

func TestLoopUsageByToolSnapshot_EmptyWhenUnused(t *testing.T) {
	var l Loop
	assert.Nil(t, l.usageByToolSnapshot(), "no tool runs -> nil, not a slice of zero buckets")
}
