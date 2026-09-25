// Package models is the canonical, provider-neutral registry of built-in model
// metadata: per-token pricing and capability flags. It is a leaf (imports only
// pkg/agent/llm — no provider SDK), so providers and downstream consumers
// (pkg/x/llmpricing, admind) can read it without pulling an SDK in.
//
// It is the single home for new per-model facts — do not add a parallel table.
// pkg/x/llmpricing projects its price maps from this registry's .Pricing field
// and stays the entry point for pricing-only consumers.
//
// Prices are USD per 1e6 tokens, verified against published list pricing as of
// 2026-06-24. Anthropic cache buckets follow its documented economics:
// cache-write = 1.25×input (5-minute TTL), cache-read = 0.1×input. OpenAI cache
// pricing is not modeled (see WithoutCacheRatios). Re-check on new model
// releases or provider price changes.
package models

import "github.com/authzed/openagentprimitives/pkg/agent/llm"

// ModelInfo carries the per-model facts this registry knows: list pricing, the
// set of provider-neutral capabilities the model supports, and the MIME types
// it accepts as native message content blocks.
type ModelInfo struct {
	// Pricing is list price in USD per 1e6 tokens; the zero value means this
	// table carries no price for the model.
	Pricing llm.ModelPricing
	// Capabilities is the provider-neutral capability set; empty means none.
	Capabilities llm.CapabilitySet
	// NativeInputMIMEs is what the model accepts as native message content
	// blocks; empty means every attachment falls back to extracted text.
	NativeInputMIMEs llm.MIMESet
}

// WithCacheRatios builds a ModelPricing from base input/output prices, deriving
// the two cache buckets from the Anthropic ratios. Used for the built-in table
// and for catalog prices (which carry only input/output).
func WithCacheRatios(inPerMTok, outPerMTok float64) llm.ModelPricing {
	return llm.ModelPricing{
		InputPerMTok:         inPerMTok,
		OutputPerMTok:        outPerMTok,
		CacheCreationPerMTok: inPerMTok * 1.25,
		CacheReadPerMTok:     inPerMTok * 0.10,
		Currency:             "USD",
	}
}

// WithoutCacheRatios builds a ModelPricing with the cache buckets left zero,
// for providers whose prompt-cache pricing this table does not model — OpenAI's
// caching economics differ from Anthropic's 1.25×/0.1×. Consumers that read
// only input/output are unaffected; anything that prices cache tokens for such
// a provider must fill the buckets first.
func WithoutCacheRatios(inPerMTok, outPerMTok float64) llm.ModelPricing {
	return llm.ModelPricing{
		InputPerMTok:  inPerMTok,
		OutputPerMTok: outPerMTok,
		Currency:      "USD",
	}
}

// nativeFiles is the capability set shared by every Anthropic model that
// supports native file input/output (see the Anthropic table below for which
// models carry it).
func nativeFiles() llm.CapabilitySet {
	return llm.NewCapabilitySet(llm.CapNativeFileOut, llm.CapNativeFileIn)
}

// anthropicNativeInput is the MIME set every current Anthropic model accepts as
// native message content: PDF and four image types, capped at 32 MB / 600 pages
// per request. Not pptx/docx/xlsx — Anthropic's skills of those names GENERATE
// those formats in a code-execution container rather than accepting them.
//
// Deliberately NOT text/plain, even though the API accepts it as a document
// source. Native passthrough exists to preserve fidelity a text extractor loses
// (layout, tables, figures); plain text has none of that and already has an
// extractor, so it would only duplicate its own extracted text. It is also what
// keeps NativeBlockDocument a one-shape guarantee: Anthropic's document source
// union has no generic "base64 + arbitrary media_type" variant, so admitting
// text/plain would force the adapter to branch on a MIME string to choose
// between two SDK source shapes. TestOnlyPDFMapsToNativeBlockDocument locks
// that in. If this is revisited, the adapter needs the second source variant
// BEFORE a text/plain row is added here — never the row first.
//
// This table is the single authority on native support: adding a format is a
// one-row edit and nothing else, a property TestNativeSupportIsRegistryDriven
// (pkg/agent/runner) asserts.
func anthropicNativeInput() llm.MIMESet {
	return llm.NewMIMESet(map[string]string{
		"application/pdf": llm.NativeBlockDocument,
		"image/jpeg":      llm.NativeBlockImage,
		"image/png":       llm.NativeBlockImage,
		"image/gif":       llm.NativeBlockImage,
		"image/webp":      llm.NativeBlockImage,
	})
}

// Anthropic is the canonical built-in model table for Anthropic models. Keep
// in sync with new model releases; pkg/x/llmpricing.Anthropic and the anthropic
// provider's Pricing project their price maps from this table's .Pricing
// field, and admind/cost derives its Anthropic display prices from it in turn.
var Anthropic = map[string]ModelInfo{
	"claude-opus-4-8": {Pricing: WithCacheRatios(5, 25), Capabilities: nativeFiles(), NativeInputMIMEs: anthropicNativeInput()},
	"claude-opus-4-7": {Pricing: WithCacheRatios(5, 25), Capabilities: nativeFiles(), NativeInputMIMEs: anthropicNativeInput()},
	"claude-opus-4-6": {Pricing: WithCacheRatios(5, 25), Capabilities: nativeFiles(), NativeInputMIMEs: anthropicNativeInput()},
	// Sonnet 5 lists at $3/$15 (regular). An intro price of $2/$10 applies
	// through 2026-08-31; we use the regular price for a durable estimate and to
	// match admind's dashboard table (both derive from here).
	"claude-sonnet-5":           {Pricing: WithCacheRatios(3, 15), Capabilities: nativeFiles(), NativeInputMIMEs: anthropicNativeInput()},
	"claude-sonnet-4-6":         {Pricing: WithCacheRatios(3, 15), Capabilities: nativeFiles(), NativeInputMIMEs: anthropicNativeInput()},
	"claude-haiku-4-5":          {Pricing: WithCacheRatios(1, 5), Capabilities: llm.NewCapabilitySet(), NativeInputMIMEs: anthropicNativeInput()},
	"claude-haiku-4-5-20251001": {Pricing: WithCacheRatios(1, 5), Capabilities: llm.NewCapabilitySet(), NativeInputMIMEs: anthropicNativeInput()},
	"claude-fable-5":            {Pricing: WithCacheRatios(10, 50), Capabilities: nativeFiles(), NativeInputMIMEs: anthropicNativeInput()},
}

// OpenAI is the canonical built-in model table for OpenAI models (gpt-5.3-codex
// is the Codex model). Cache buckets are intentionally unmodeled — see
// WithoutCacheRatios. No OpenAI model carries a capability flag today.
// Source: https://developers.openai.com/api/docs/pricing
var OpenAI = map[string]ModelInfo{
	"gpt-5.5":       {Pricing: WithoutCacheRatios(5.00, 30.00), Capabilities: llm.NewCapabilitySet()},
	"gpt-5.5-pro":   {Pricing: WithoutCacheRatios(30.00, 180.00), Capabilities: llm.NewCapabilitySet()},
	"gpt-5.4":       {Pricing: WithoutCacheRatios(2.50, 15.00), Capabilities: llm.NewCapabilitySet()},
	"gpt-5.4-mini":  {Pricing: WithoutCacheRatios(0.75, 4.50), Capabilities: llm.NewCapabilitySet()},
	"gpt-5.4-nano":  {Pricing: WithoutCacheRatios(0.20, 1.25), Capabilities: llm.NewCapabilitySet()},
	"gpt-5.4-pro":   {Pricing: WithoutCacheRatios(30.00, 180.00), Capabilities: llm.NewCapabilitySet()},
	"gpt-5.3-codex": {Pricing: WithoutCacheRatios(1.75, 14.00), Capabilities: llm.NewCapabilitySet()},
}

// OpenRouter is the canonical built-in model table for the OpenRouter provider.
// OpenRouter is a routing gateway over many upstream providers, so entries here
// are a convenience, not load-bearing: its Send captures the per-response
// provider-reported dollar cost (llm.Usage.CostUSD) from the API response,
// which is authoritative over this table. Deliberately sparse — a few
// well-known upstream ids plus the "openrouter/auto" virtual model, which has
// no fixed price because every response routes elsewhere. A missing entry is
// not an error; see openrouter.Provider.Pricing's ok=false contract.
var OpenRouter = map[string]ModelInfo{
	"openrouter/auto":             {Capabilities: llm.NewCapabilitySet()},
	"anthropic/claude-3.5-sonnet": {Pricing: WithoutCacheRatios(3.00, 15.00), Capabilities: llm.NewCapabilitySet()},
	"openai/gpt-5.4":              {Pricing: WithoutCacheRatios(2.50, 15.00), Capabilities: llm.NewCapabilitySet()},
}

// byProvider is the ordered provider-name → built-in-table mapping, and the
// single list the cross-provider helpers below project from: Tables drops the
// names, ProviderFor searches them. Adding a provider is one row.
var byProvider = []struct {
	provider string
	models   map[string]ModelInfo
}{
	{provider: "anthropic", models: Anthropic},
	{provider: "openai", models: OpenAI},
	{provider: "openrouter", models: OpenRouter},
}

// Tables returns every built-in per-provider model table. It is the seam for
// consumers that want all built-in models regardless of provider (e.g.
// pkg/x/llmpricing.Tables, which projects each entry's .Pricing); adding a row
// to byProvider makes it flow to those consumers without further edits.
func Tables() []map[string]ModelInfo {
	out := make([]map[string]ModelInfo, 0, len(byProvider))
	for _, t := range byProvider {
		out = append(out, t.models)
	}
	return out
}

// Providers returns the providers that have a built-in model table, in table
// order. Membership means "a real upstream service, with its own API key and
// its own model ids" — which is what lets a caller tell those apart from a
// provider name that names no upstream at all, such as the "test" harness
// provider the e2e suite pairs with a scripted model.
func Providers() []string {
	out := make([]string, 0, len(byProvider))
	for _, t := range byProvider {
		out = append(out, t.provider)
	}
	return out
}

// ProviderFor reports the provider whose built-in table lists id, so a caller
// holding a user-supplied (provider, model) pair can refuse a pairing that is
// definitively wrong — a bare "claude-sonnet-5" under openai, an
// "openrouter/auto" under anthropic — before it is registered as a cluster
// default and fails at the agent's first turn.
//
// known=false means the id is NOT positively attributable to one provider, and
// a caller MUST NOT refuse a pairing on that basis. Two cases reach it:
//
//   - No table lists the id. These tables trail new releases and OpenRouter's
//     is deliberately sparse (see its doc), so an unrecognized id is routinely
//     a perfectly good model. Refusing one would block a legitimate config,
//     which is worse than the mispairing this exists to catch.
//   - More than one table lists it. Then the id says nothing about which
//     provider was meant, so no pairing involving it can be called wrong. No
//     id collides across the built-in tables today — TestTables_
//     EnumeratesEveryProvider asserts exactly that — so this arm is the
//     defensive half of the contract, not a live case.
//
// This is attribution, not validation: a known result says where an id is
// known from, never that a provider cannot serve some other id.
func ProviderFor(id string) (string, bool) {
	found := ""
	for _, t := range byProvider {
		if _, ok := t.models[id]; !ok {
			continue
		}
		if found != "" {
			return "", false
		}
		found = t.provider
	}
	return found, found != ""
}
