// Package llmpricing is the pricing-only entry point onto the canonical
// per-model registry in pkg/agent/llm/models, whose tables it projects. It is
// a leaf — no provider SDK — so the LLM providers and the admin dashboard
// (admind/cost) can both read prices without pulling one, and no consumer
// holds price literals of its own. New per-model facts belong in
// pkg/agent/llm/models, not here.
//
// Prices are USD per 1e6 tokens, verified against each provider's published
// list pricing (Anthropic as of 2026-06-24). Anthropic cache buckets follow
// its documented prompt-caching economics: cache-write = 1.25×input (5-minute
// TTL), cache-read = 0.1×input. OpenAI cache pricing is not modeled (see
// WithoutCacheRatios). Re-check on new model releases or price changes.
package llmpricing

import (
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/models"
)

// WithCacheRatios builds a ModelPricing from base input/output prices, deriving
// the two cache buckets from the Anthropic ratios. Used for the built-in table
// and for catalog prices (which carry only input/output).
func WithCacheRatios(inPerMTok, outPerMTok float64) llm.ModelPricing {
	return models.WithCacheRatios(inPerMTok, outPerMTok)
}

// WithoutCacheRatios builds a ModelPricing with the cache buckets left zero,
// for providers whose prompt-cache economics this table does not model — e.g.
// OpenAI, which differs from Anthropic's 1.25×/0.1×. Consumers reading only
// input/output are unaffected; anything that starts billing cache tokens for
// such a provider must fill the buckets first.
func WithoutCacheRatios(inPerMTok, outPerMTok float64) llm.ModelPricing {
	return models.WithoutCacheRatios(inPerMTok, outPerMTok)
}

// projectPricing maps a models.ModelInfo table down to the pricing-only shape
// this package exposes.
//
// Invariant: a zero price is never a known price. Presence in a projected
// table means "this model's price is known", and every consumer treats
// presence as "safe to bill at this rate" — so an all-zero entry is dropped
// rather than carried through as $0.00. "openrouter/auto" is the motivating
// case: its real cost is per-response provider-reported, so surviving the
// projection would fabricate a $0.00 estimate for every routed session
// instead of falling back to the unknown-price path.
func projectPricing(table map[string]models.ModelInfo) map[string]llm.ModelPricing {
	out := make(map[string]llm.ModelPricing, len(table))
	for id, info := range table {
		p := info.Pricing
		if p.InputPerMTok == 0 && p.OutputPerMTok == 0 && p.CacheCreationPerMTok == 0 && p.CacheReadPerMTok == 0 {
			continue
		}
		out[id] = p
	}
	return out
}

// Anthropic is the built-in price table for Anthropic models, read by the
// anthropic provider's Pricing and by admind/cost. Add new models in
// pkg/agent/llm/models.Anthropic.
var Anthropic = projectPricing(models.Anthropic)

// OpenAI is the built-in price table for OpenAI models, read by the openai
// provider's Pricing and by admind/cost. Cache buckets are intentionally
// unmodeled — see WithoutCacheRatios.
var OpenAI = projectPricing(models.OpenAI)

// OpenRouter is the built-in price table for OpenRouter models. Deliberately
// sparse: the provider's per-response reported cost (llm.Usage.CostUSD) is
// authoritative, and this only backstops a static estimate for a few
// well-known ids before any reported cost exists.
var OpenRouter = projectPricing(models.OpenRouter)

// Tables returns every built-in per-provider price table — the seam for
// consumers wanting all built-in-priced models regardless of provider, so a
// new table in pkg/agent/llm/models reaches them without further edits. A
// provider's own Pricing deliberately does NOT use this: it reads only its own
// table, so it can never return another provider's price.
func Tables() []map[string]llm.ModelPricing {
	return []map[string]llm.ModelPricing{Anthropic, OpenAI, OpenRouter}
}
