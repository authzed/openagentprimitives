package llmpricing

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

func TestWithCacheRatios(t *testing.T) {
	p := WithCacheRatios(5, 25)
	assert.Equal(t, 5.0, p.InputPerMTok)
	assert.Equal(t, 25.0, p.OutputPerMTok)
	assert.Equal(t, 6.25, p.CacheCreationPerMTok) // 1.25×5
	assert.Equal(t, 0.5, p.CacheReadPerMTok)      // 0.1×5
	assert.Equal(t, "USD", p.Currency)
}

func TestWithoutCacheRatios(t *testing.T) {
	p := WithoutCacheRatios(5, 30)
	assert.Equal(t, 5.0, p.InputPerMTok)
	assert.Equal(t, 30.0, p.OutputPerMTok)
	assert.Zero(t, p.CacheCreationPerMTok, "OpenAI cache pricing is intentionally unmodeled")
	assert.Zero(t, p.CacheReadPerMTok, "OpenAI cache pricing is intentionally unmodeled")
	assert.Equal(t, "USD", p.Currency)
}

func TestAnthropicKnownModels(t *testing.T) {
	o, ok := Anthropic["claude-opus-4-8"]
	assert.True(t, ok)
	assert.Equal(t, 5.0, o.InputPerMTok)
	assert.Equal(t, 25.0, o.OutputPerMTok)
	_, ok = Anthropic["gpt-5.5"]
	assert.False(t, ok, "OpenAI models live in the OpenAI table, not the Anthropic one")
}

func TestOpenAIKnownModels(t *testing.T) {
	c, ok := OpenAI["gpt-5.3-codex"]
	assert.True(t, ok, "the Codex model is priced")
	assert.Equal(t, 1.75, c.InputPerMTok)
	assert.Equal(t, 14.0, c.OutputPerMTok)
	assert.Zero(t, c.CacheReadPerMTok, "OpenAI cache pricing is unmodeled")
	_, ok = OpenAI["claude-opus-4-8"]
	assert.False(t, ok, "Anthropic models live in the Anthropic table, not the OpenAI one")
}

// TestOpenRouter_UnpricedModelsExcludedFromPricing pins the "a zero price is
// never a known price" invariant (projectPricing): openrouter/auto carries
// the zero Pricing value in pkg/agent/llm/models (its real cost is
// per-response provider-reported, not fixed), so it must be ABSENT from the
// projected pricing table — present-with-zero would let downstream consumers
// (the runner cost hook, admind's PriceMap) treat it as a known $0.00 price.
// A priced OpenRouter entry must still survive the projection unchanged.
func TestOpenRouter_UnpricedModelsExcludedFromPricing(t *testing.T) {
	c, ok := OpenRouter["anthropic/claude-3.5-sonnet"]
	assert.True(t, ok, "a priced upstream id survives the projection")
	assert.Equal(t, 3.00, c.InputPerMTok)
	_, ok = OpenRouter["openrouter/auto"]
	assert.False(t, ok, "the zero-priced auto-router virtual model must be excluded from the pricing projection")
}

func TestTablesEnumeratesEveryProvider(t *testing.T) {
	seen := map[string]llm.ModelPricing{}
	for _, table := range Tables() {
		for id, p := range table {
			seen[id] = p
		}
	}
	assert.Contains(t, seen, "claude-opus-4-8", "Tables must include the Anthropic table")
	assert.Contains(t, seen, "gpt-5.3-codex", "Tables must include the OpenAI table")
	assert.NotContains(t, seen, "openrouter/auto", "the zero-priced auto-router model is excluded from pricing projections")
	assert.Len(t, seen, len(Anthropic)+len(OpenAI)+len(OpenRouter), "Tables must expose every PRICED entry (no id collisions across providers)")
}
