package cost

import (
	"context"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/x/llmpricing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// openRouterPricer looks a bare model id up in the real canonical
// pkg/x/llmpricing.OpenRouter table — used (rather than a hand-rolled fake) so
// TestEval_ByModel_OpenRouterAutoBucket_UnknownNotZero pins the actual FIX A
// behavior (projectPricing excluding the zero-priced "openrouter/auto" entry)
// rather than an assumption about it.
func openRouterPricer(model string) (llm.ModelPricing, bool) {
	p, ok := llmpricing.OpenRouter[model]
	return p, ok
}

func opusPricer(model string) (llm.ModelPricing, bool) {
	if model == "claude-opus-4-8" {
		return llm.ModelPricing{InputPerMTok: 15, OutputPerMTok: 75, CacheCreationPerMTok: 18.75, CacheReadPerMTok: 1.5, Currency: "USD"}, true
	}
	return llm.ModelPricing{}, false
}

// multiModelPricer serves two bare model ids at different rates, for the
// ByModel bucket-pricing tests below; "mystery-model" stays deliberately
// unpriced so a bucket can hit the unknown-price path.
func multiModelPricer(model string) (llm.ModelPricing, bool) {
	switch model {
	case "anthropic/claude-3.5-sonnet":
		return llm.ModelPricing{InputPerMTok: 3, OutputPerMTok: 15, Currency: "USD"}, true
	case "openai/gpt-4o":
		return llm.ModelPricing{InputPerMTok: 2.5, OutputPerMTok: 10, Currency: "USD"}, true
	default:
		return llm.ModelPricing{}, false
	}
}

func newWithCapture() (*Reporter, *v1.EstimatedSessionCost) {
	return newWithCapturePricer(opusPricer)
}

// newWithCapturePricer is newWithCapture parameterized on the pricing func,
// for tests that need a table serving more than one model (the ByModel
// bucket tests below).
func newWithCapturePricer(pricer func(string) (llm.ModelPricing, bool)) (*Reporter, *v1.EstimatedSessionCost) {
	var stamped v1.EstimatedSessionCost
	r := New(Deps{
		Pricing: pricer,
		Stamp:   func(_ context.Context, c v1.EstimatedSessionCost) error { stamped = c; return nil },
		Now:     func() metav1.Time { return metav1.Time{} },
	})
	return r, &stamped
}

func endInput(reason string, in, out, cc, cr int64) pipeline.Input {
	return pipeline.Input{End: &pipeline.SessionEndInfo{
		Reason: reason, Model: "claude-opus-4-8",
		InputTokens: in, OutputTokens: out, CacheCreationTokens: cc, CacheReadTokens: cr,
	}}
}

func TestEval_StampError_StillMessages(t *testing.T) {
	// A failed status stamp must NOT block teardown and must NOT suppress the
	// completion/failure message (the message doesn't depend on the stamp).
	r := New(Deps{
		Pricing: opusPricer,
		Stamp:   func(_ context.Context, _ v1.EstimatedSessionCost) error { return errors.New("boom") },
		Now:     func() metav1.Time { return metav1.Time{} },
	})
	dec := r.Eval(context.Background(), endInput("completed", 1_000_000, 0, 0, 0))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, dec.Notices, 1, "stamp error must not suppress the completion message")
	assert.Contains(t, dec.Notices[0].Notice.Args().Lead, "$")
}

func TestEval_Completed_MessageAndStamp(t *testing.T) {
	r, stamped := newWithCapture()
	// 1,000,000 in × $15 + 1,000,000 out × $75 = $90.00 → 90,000,000 microUSD.
	dec := r.Eval(context.Background(), endInput("completed", 1_000_000, 1_000_000, 0, 0))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, dec.Notices, 1, "completed → message")
	assert.Contains(t, dec.Notices[0].Notice.Args().Lead, "$90.00")
	assert.Equal(t, int64(90_000_000), stamped.AmountMicroUSD)
	assert.True(t, stamped.PricingKnown)
}

func TestEval_Idle_StampNoMessage(t *testing.T) {
	r, stamped := newWithCapture()
	dec := r.Eval(context.Background(), endInput("idle", 1_000_000, 0, 0, 0))
	assert.Empty(t, dec.Notices, "idle → no message")
	assert.Equal(t, int64(15_000_000), stamped.AmountMicroUSD, "but stamp still written")
}

func TestEval_Failed_HasMessage(t *testing.T) {
	r, _ := newWithCapture()
	dec := r.Eval(context.Background(), endInput("failed", 1_000_000, 0, 0, 0))
	assert.Len(t, dec.Notices, 1, "failed → message")
}

func TestEval_UnknownModel_NoFabrication(t *testing.T) {
	r, stamped := newWithCapture()
	in := endInput("completed", 1_000_000, 0, 0, 0)
	in.End.Model = "mystery-model"
	dec := r.Eval(context.Background(), in)
	require.Len(t, dec.Notices, 1)
	assert.Contains(t, dec.Notices[0].Notice.Args().Lead, "unavailable")
	assert.False(t, stamped.PricingKnown)
	assert.Equal(t, int64(0), stamped.AmountMicroUSD)
}

func TestEval_CacheAware(t *testing.T) {
	r, stamped := newWithCapture()
	// 1M cache-read × $1.5 = $1.50 → 1,500,000 microUSD.
	r.Eval(context.Background(), endInput("completed", 0, 0, 0, 1_000_000))
	assert.Equal(t, int64(1_500_000), stamped.AmountMicroUSD)
}

func TestName_Points(t *testing.T) {
	r, _ := newWithCapture()
	assert.Equal(t, "session_cost", r.Name())
	assert.Equal(t, []pipeline.Point{pipeline.SessionEnd}, r.Points())
}

// --- Per-served-model cost buckets (Task 8) ---

func TestEval_ByModel_ReportedPreferredOverEstimated(t *testing.T) {
	r, stamped := newWithCapturePricer(multiModelPricer)
	in := endInput("completed", 0, 0, 0, 0)
	in.End.ByModel = []pipeline.ModelUsage{
		{ // reported cost wins even though a table price also exists for this model.
			Display: "openrouter/anthropic/claude-3.5-sonnet", Model: "anthropic/claude-3.5-sonnet",
			InputTokens: 1_000_000, OutputTokens: 100_000,
			ReportedCostMicroUSD: 12_345, CostReported: true,
		},
		{ // no reported cost -> priced via table: 1M in x $2.5 + 1M out x $10 = $12.50.
			Display: "openrouter/openai/gpt-4o", Model: "openai/gpt-4o",
			InputTokens: 1_000_000, OutputTokens: 1_000_000,
		},
	}
	dec := r.Eval(context.Background(), in)
	assert.Equal(t, pipeline.Allow, dec.Verdict)

	require.Len(t, stamped.ByModel, 2)
	assert.Equal(t, "openrouter/anthropic/claude-3.5-sonnet", stamped.ByModel[0].Model)
	assert.Equal(t, int64(12_345), stamped.ByModel[0].AmountMicroUSD, "reported cost preferred over table pricing")
	assert.True(t, stamped.ByModel[0].PricingKnown)

	assert.Equal(t, "openrouter/openai/gpt-4o", stamped.ByModel[1].Model)
	assert.Equal(t, int64(12_500_000), stamped.ByModel[1].AmountMicroUSD, "unreported bucket priced via the table")
	assert.True(t, stamped.ByModel[1].PricingKnown)

	assert.Equal(t, int64(12_345+12_500_000), stamped.AmountMicroUSD, "session total = sum of buckets")
	assert.True(t, stamped.PricingKnown, "every bucket priced -> session PricingKnown")
}

func TestEval_ByModel_UnknownPriceBucket_ZeroAndUnknown(t *testing.T) {
	r, stamped := newWithCapturePricer(multiModelPricer)
	in := endInput("completed", 0, 0, 0, 0)
	in.End.ByModel = []pipeline.ModelUsage{
		{Display: "openrouter/mystery/model", Model: "mystery-model", InputTokens: 1_000_000, OutputTokens: 1_000_000},
	}
	r.Eval(context.Background(), in)

	require.Len(t, stamped.ByModel, 1)
	assert.Equal(t, int64(0), stamped.ByModel[0].AmountMicroUSD, "unknown price + unreported -> 0, no fabrication")
	assert.False(t, stamped.ByModel[0].PricingKnown)
	assert.Equal(t, int64(0), stamped.AmountMicroUSD)
	assert.False(t, stamped.PricingKnown, "one unknown bucket -> session PricingKnown=false")
}

// TestEval_ByModel_OpenRouterAutoBucket_UnknownNotZero is the FIX A runner-side
// pin: "openrouter/auto" is deliberately zero-priced in
// pkg/agent/llm/models (its real cost is per-response provider-reported), and
// pkg/x/llmpricing.projectPricing now excludes zero-priced entries from the
// PRICING table entirely (a zero price is never a known price). An
// UNREPORTED bucket (no provider-reported cost, e.g. the routed request never
// got a per-turn cost back) served by the bare model "openrouter/auto" must
// therefore price as UNKNOWN — PricingKnown=false, AmountMicroUSD=0 — never a
// fabricated $0-known amount.
func TestEval_ByModel_OpenRouterAutoBucket_UnknownNotZero(t *testing.T) {
	r, stamped := newWithCapturePricer(openRouterPricer)
	in := endInput("completed", 0, 0, 0, 0)
	in.End.ByModel = []pipeline.ModelUsage{
		{Display: "openrouter/openrouter/auto", Model: "openrouter/auto", InputTokens: 1_000_000, OutputTokens: 1_000_000},
	}
	r.Eval(context.Background(), in)

	require.Len(t, stamped.ByModel, 1)
	assert.False(t, stamped.ByModel[0].PricingKnown, "openrouter/auto has no fixed price -> unknown, not a fabricated $0")
	assert.Equal(t, int64(0), stamped.ByModel[0].AmountMicroUSD)
	assert.False(t, stamped.PricingKnown, "the one unpriced bucket -> session PricingKnown=false")
	assert.Equal(t, int64(0), stamped.AmountMicroUSD)
}

func TestEval_ByModel_NilPricingDep_NoPanic(t *testing.T) {
	r := New(Deps{Stamp: func(_ context.Context, _ v1.EstimatedSessionCost) error { return nil }})
	in := endInput("completed", 0, 0, 0, 0)
	in.End.ByModel = []pipeline.ModelUsage{{Display: "x/y", Model: "y", InputTokens: 1}}
	assert.NotPanics(t, func() { r.Eval(context.Background(), in) })
}

// TestEval_ByModel_SingleBucketMatchesLegacyTotal is the required invariant:
// a single unreported-cost bucket must price to EXACTLY the legacy
// session-total amount for identical tokens, proving the microUSD/
// buildCostBuckets refactor shares one rate-math implementation rather than
// having quietly diverged into two.
func TestEval_ByModel_SingleBucketMatchesLegacyTotal(t *testing.T) {
	legacy, legacyStamped := newWithCapture()
	legacy.Eval(context.Background(), endInput("completed", 1_000_000, 200_000, 10_000, 50_000))

	bucketed, bucketedStamped := newWithCapture()
	in := endInput("completed", 1_000_000, 200_000, 10_000, 50_000)
	in.End.ByModel = []pipeline.ModelUsage{{
		Display: "anthropic/claude-opus-4-8", Model: "claude-opus-4-8",
		InputTokens: 1_000_000, OutputTokens: 200_000, CacheCreationTokens: 10_000, CacheReadTokens: 50_000,
	}}
	bucketed.Eval(context.Background(), in)

	require.True(t, legacyStamped.PricingKnown)
	require.Len(t, bucketedStamped.ByModel, 1)
	assert.Equal(t, legacyStamped.AmountMicroUSD, bucketedStamped.ByModel[0].AmountMicroUSD,
		"single unreported bucket must price identically to the legacy session total for the same tokens")
	assert.Equal(t, legacyStamped.AmountMicroUSD, bucketedStamped.AmountMicroUSD,
		"session total with one bucket == that bucket's amount")
}
