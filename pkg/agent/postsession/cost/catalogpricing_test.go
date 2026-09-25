package cost

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCatalogFirstPricing_PrefersCatalogOverProvider verifies the catalog
// price (operator-authoritative, same source the admin dashboard uses) wins
// over the provider's built-in table for the session's CONFIGURED model —
// this is what keeps the session cost estimate consistent with the dashboard
// figure. l.Model is set to the model under test: the catalog rate is scoped
// to the one model the admin was looking at when they set it (see
// catalogFirstPricing's doc), so this pins that "configured model" identity.
func TestCatalogFirstPricing_PrefersCatalogOverProvider(t *testing.T) {
	p := fake.New(nil)
	p.SetPricing(map[string]llm.ModelPricing{
		"claude-opus-4-8": {InputPerMTok: 999, OutputPerMTok: 999, Currency: "USD"},
	})
	l := &runner.Loop{
		Provider:           p,
		Model:              "claude-opus-4-8",
		ModelInputPerMTok:  7,
		ModelOutputPerMTok: 35,
	}

	pricing := catalogFirstPricing(l)
	got, ok := pricing("claude-opus-4-8")
	require.True(t, ok)
	assert.Equal(t, 7.0, got.InputPerMTok)
	assert.Equal(t, 35.0, got.OutputPerMTok)
	assert.InDelta(t, 8.75, got.CacheCreationPerMTok, 1e-9, "1.25× the catalog input price")
	assert.InDelta(t, 0.7, got.CacheReadPerMTok, 1e-9, "0.1× the catalog input price")
}

// TestCatalogFirstPricing_FallsBackToProviderWhenCatalogUnpriced verifies a
// Loop with no catalog price (0/0, e.g. legacy or BYO model resolution)
// defers entirely to the provider's built-in table.
func TestCatalogFirstPricing_FallsBackToProviderWhenCatalogUnpriced(t *testing.T) {
	p := fake.New(nil)
	p.SetPricing(map[string]llm.ModelPricing{
		"claude-opus-4-8": {InputPerMTok: 5, OutputPerMTok: 25, Currency: "USD"},
	})
	l := &runner.Loop{Provider: p, Model: "claude-opus-4-8"}

	pricing := catalogFirstPricing(l)
	got, ok := pricing("claude-opus-4-8")
	require.True(t, ok)
	assert.Equal(t, 5.0, got.InputPerMTok)
	assert.Equal(t, 25.0, got.OutputPerMTok)
}

// TestCatalogFirstPricing_OtherServedModel_FallsThroughToProvider verifies
// the Task 8 fix: a catalog-priced session (ModelInputPerMTok/OutputPerMTok
// set for l.Model, "openrouter/auto") asked to price a DIFFERENT
// actually-served model (an OpenRouter per-bucket served id) must NOT inherit
// the configured model's fixed catalog rate — it falls through to the
// provider's own table, same as the unpriced-catalog case, and must resolve
// to a rate distinct from the catalog's.
func TestCatalogFirstPricing_OtherServedModel_FallsThroughToProvider(t *testing.T) {
	p := fake.New(nil)
	p.SetPricing(map[string]llm.ModelPricing{
		"anthropic/claude-3.5-sonnet": {InputPerMTok: 3, OutputPerMTok: 15, Currency: "USD"},
	})
	l := &runner.Loop{
		Provider:           p,
		Model:              "openrouter/auto",
		ModelInputPerMTok:  7,
		ModelOutputPerMTok: 35,
	}

	pricing := catalogFirstPricing(l)

	// The configured model still prices from the catalog (unchanged).
	configured, ok := pricing("openrouter/auto")
	require.True(t, ok)
	assert.Equal(t, 7.0, configured.InputPerMTok)
	assert.Equal(t, 35.0, configured.OutputPerMTok)

	// A different served model resolves through the PROVIDER's table — a
	// rate distinct from (not equal to) the catalog's fixed rate.
	other, ok := pricing("anthropic/claude-3.5-sonnet")
	require.True(t, ok)
	assert.Equal(t, 3.0, other.InputPerMTok)
	assert.Equal(t, 15.0, other.OutputPerMTok)
	assert.NotEqual(t, configured.InputPerMTok, other.InputPerMTok, "must not inherit the configured model's catalog rate")
}

// TestCatalogFirstPricing_OtherServedModel_UnknownToProvider verifies the
// fallthrough propagates ok=false (no fabrication) when the provider's own
// table has no entry for the other served model either — the catalog price
// must not paper over an unpriced non-configured model.
func TestCatalogFirstPricing_OtherServedModel_UnknownToProvider(t *testing.T) {
	p := fake.New(nil) // no pricing configured at all
	l := &runner.Loop{
		Provider:           p,
		Model:              "openrouter/auto",
		ModelInputPerMTok:  7,
		ModelOutputPerMTok: 35,
	}

	pricing := catalogFirstPricing(l)
	_, ok := pricing("mystery/model")
	assert.False(t, ok, "unpriced non-configured model must propagate ok=false, not fabricate the catalog rate")
}
