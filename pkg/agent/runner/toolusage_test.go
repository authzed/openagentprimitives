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
	l.addToolCost("claude-oauth", 5.43, true)
	l.addToolCost("claude-oauth", 1.31, true)
	l.addToolCost("aider", 2.10, true)

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
	l.addToolCost("claude-oauth", -1.0, true)

	got := l.usageByToolSnapshot()
	require.Len(t, got, 1, "the tool is still recorded")
	assert.False(t, got[0].CostReported, "negative cost -> treated as unreported")
	assert.Equal(t, int64(0), got[0].CostMicroUSD, "no negative amount is ever added")
}

func TestLoopAddToolCost_ZeroCostUnbilled(t *testing.T) {
	// An unbilled / credential-halt run reports cost 0; it adds 0 and does not
	// mark the bucket as having a reported (billed) cost.
	var l Loop
	l.addToolCost("claude-oauth", 0, false)

	got := l.usageByToolSnapshot()
	require.Len(t, got, 1)
	assert.Equal(t, int64(0), got[0].CostMicroUSD)
}

func TestLoopUsageByToolSnapshot_EmptyWhenUnused(t *testing.T) {
	var l Loop
	assert.Nil(t, l.usageByToolSnapshot(), "no tool runs -> nil, not a slice of zero buckets")
}
