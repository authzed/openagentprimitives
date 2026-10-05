package runner

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// costUSD is a small helper for building an llm.Usage.CostUSD pointer inline.
func costUSD(v float64) *float64 { return &v }

func TestLoopAddModelUsage_AccumulatesPerDisplay(t *testing.T) {
	var l Loop
	// Two turns served by the same model (accumulate into one bucket), one
	// turn served by a different model under OpenRouter-style auto-routing
	// (a distinct bucket) — this is the "multi-turn multi-model" shape Task 8
	// exists to attribute correctly.
	l.addModelUsage("openrouter/anthropic/claude-3.5-sonnet", "anthropic/claude-3.5-sonnet",
		llm.Usage{InputTokens: 100, OutputTokens: 10, CacheCreationTokens: 1, CacheReadTokens: 2, CostUSD: costUSD(0.01)})
	l.addModelUsage("openrouter/anthropic/claude-3.5-sonnet", "anthropic/claude-3.5-sonnet",
		llm.Usage{InputTokens: 50, OutputTokens: 5, CostUSD: costUSD(0.02)})
	l.addModelUsage("openrouter/openai/gpt-4o", "openai/gpt-4o",
		llm.Usage{InputTokens: 200, OutputTokens: 20})

	got := l.usageByModelSnapshot()
	require.Len(t, got, 2, "two distinct display ids -> two buckets, in first-encountered order")

	assert.Equal(t, "openrouter/anthropic/claude-3.5-sonnet", got[0].Display)
	assert.Equal(t, "anthropic/claude-3.5-sonnet", got[0].Model)
	assert.Equal(t, int64(150), got[0].InputTokens)
	assert.Equal(t, int64(15), got[0].OutputTokens)
	assert.Equal(t, int64(1), got[0].CacheCreationTokens)
	assert.Equal(t, int64(2), got[0].CacheReadTokens)
	assert.True(t, got[0].CostReported)
	assert.Equal(t, int64(30_000), got[0].ReportedCostMicroUSD, "0.01+0.02 USD -> 30,000 micro-USD")

	assert.Equal(t, "openrouter/openai/gpt-4o", got[1].Display)
	assert.Equal(t, "openai/gpt-4o", got[1].Model)
	assert.Equal(t, int64(200), got[1].InputTokens)
	assert.Equal(t, int64(20), got[1].OutputTokens)
	assert.False(t, got[1].CostReported, "no CostUSD reported for this turn")
	assert.Equal(t, int64(0), got[1].ReportedCostMicroUSD)
}

func TestLoopAddModelUsage_NegativeCostUSDIgnored(t *testing.T) {
	// A buggy or malicious upstream reporting a negative usage.cost must not
	// reduce the session's reported total. The turn's cost is treated as
	// unreported so cost.go's CostReported gate falls back to table pricing
	// for this bucket, rather than netting a negative amount against real
	// charges.
	var l Loop
	l.addModelUsage("openrouter/some-vendor/some-model", "some-vendor/some-model",
		llm.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: costUSD(-1.0)})

	got := l.usageByModelSnapshot()
	require.Len(t, got, 1)
	assert.Equal(t, int64(100), got[0].InputTokens, "token accounting is unaffected by the cost clamp")
	assert.Equal(t, int64(10), got[0].OutputTokens)
	assert.False(t, got[0].CostReported, "negative CostUSD -> turn's cost treated as unreported")
	assert.Equal(t, int64(0), got[0].ReportedCostMicroUSD, "no negative amount is ever added to the bucket")
}

func TestLoopUsageByModelSnapshot_EmptyWhenUnused(t *testing.T) {
	var l Loop
	assert.Nil(t, l.usageByModelSnapshot(), "no turns served -> no per-model breakdown, not a slice of zero buckets")
}

func TestFireSessionEnd_PopulatesByModel(t *testing.T) {
	cr := &endCapturingRunner{}
	l := &Loop{Model: "openrouter/auto"}
	l.pipelineExec = cr
	l.pipelineOnce.Do(func() {})

	l.addModelUsage("openrouter/anthropic/claude-3.5-sonnet", "anthropic/claude-3.5-sonnet",
		llm.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: costUSD(0.05)})

	l.fireSessionEnd(context.Background(), "completed")

	require.NotNil(t, cr.in.End)
	require.Len(t, cr.in.End.ByModel, 1)
	b := cr.in.End.ByModel[0]
	assert.Equal(t, "openrouter/anthropic/claude-3.5-sonnet", b.Display)
	assert.Equal(t, "anthropic/claude-3.5-sonnet", b.Model)
	assert.Equal(t, int64(100), b.InputTokens)
	assert.Equal(t, int64(10), b.OutputTokens)
	assert.True(t, b.CostReported)
	assert.Equal(t, int64(50_000), b.ReportedCostMicroUSD)
}

func TestFireSessionEnd_PopulatesByTool(t *testing.T) {
	cr := &endCapturingRunner{}
	l := &Loop{Model: "claude-sonnet-5"}
	l.pipelineExec = cr
	l.pipelineOnce.Do(func() {})

	l.AddToolCost("claude-oauth", 5.43, true)
	l.fireSessionEnd(context.Background(), "completed")

	require.NotNil(t, cr.in.End)
	require.Len(t, cr.in.End.ByTool, 1)
	assert.Equal(t, "claude-oauth", cr.in.End.ByTool[0].Tool)
	assert.Equal(t, int64(5_430_000), cr.in.End.ByTool[0].CostMicroUSD)
}
