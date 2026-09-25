package openrouter

import (
	"encoding/json"

	"github.com/openai/openai-go/v3/option"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// extraBodyFieldOrder fixes the iteration order routingOptions applies
// extraBodyFields in. The order has no effect on the resulting request body —
// each key is set independently — but a fixed order keeps request logs and
// (if ever added) request-level tracing reproducible across calls.
var extraBodyFieldOrder = []string{"usage", "models", "provider", "plugins"}

// extraBodyFields builds the request-body fields OpenRouter needs on top of
// openaicompat.BuildParams: usage.include (always, so Send can read back the
// reported cost — see reportedCostUSD), the routing subconfig
// (models[]/provider{}), and auto-router plugin hints. hasTools is the
// request's own Tools presence, independent of r, and feeds providerObject's
// RequireParameters default so provider{} can be emitted even when r is nil.
//
// Deliberately returns a plain map rather than option.RequestOptions: those
// wrap an unexported SDK-internal type that cannot be applied or inspected
// from outside that module, so a map is the only unit-testable shape.
func extraBodyFields(r *llm.OpenRouterRouting, hasTools bool) map[string]any {
	fields := map[string]any{"usage": map[string]any{"include": true}}
	if r != nil {
		if len(r.Models) > 0 {
			fields["models"] = r.Models
		}
		if plugins := autoPlugins(r); plugins != nil {
			fields["plugins"] = plugins
		}
	}
	if prov := providerObject(r, hasTools); prov != nil {
		fields["provider"] = prov
	}
	return fields
}

// routingOptions translates extraBodyFields into the option.RequestOption list
// Send forwards to openaicompat.RunStream.
//
// option.WithJSONSet is the only openai-go/v3 mechanism for injecting an extra
// request-body field — it sets the body's JSON value at an sjson-format key.
// oa.ChatCompletionNewParams has no SetExtraFields equivalent.
func routingOptions(r *llm.OpenRouterRouting, hasTools bool) []option.RequestOption {
	fields := extraBodyFields(r, hasTools)
	opts := make([]option.RequestOption, 0, len(fields))
	for _, key := range extraBodyFieldOrder {
		if v, ok := fields[key]; ok {
			opts = append(opts, option.WithJSONSet(key, v))
		}
	}
	return opts
}

// providerObject maps the llm.OpenRouterRouting provider-preference fields
// onto OpenRouter's `provider` request-body object. r may be nil (no routing
// configured at all) — RequireParameters' tools-default below still needs to
// run in that case. Returns nil when none of the fields end up set
// (routingOptions then omits the key entirely).
func providerObject(r *llm.OpenRouterRouting, hasTools bool) map[string]any {
	m := map[string]any{}
	if r != nil {
		if r.Sort != "" {
			m["sort"] = r.Sort
		}
		if len(r.Order) > 0 {
			m["order"] = r.Order
		}
		if len(r.Only) > 0 {
			m["only"] = r.Only
		}
		if len(r.Ignore) > 0 {
			m["ignore"] = r.Ignore
		}
		if r.AllowFallbacks != nil {
			m["allow_fallbacks"] = *r.AllowFallbacks
		}
		if r.RequireParameters != nil {
			m["require_parameters"] = *r.RequireParameters
		}
		if r.DataCollection != "" {
			m["data_collection"] = r.DataCollection
		}
		if maxPrice := maxPriceObject(r.MaxPrice); maxPrice != nil {
			m["max_price"] = maxPrice
		}
	}
	// RequireParameters defaults TRUE when the request carries tools: agents need
	// tools, so auto/fallback must only route to providers that support them
	// (OpenRouter's own wire default is false). An explicit value set above —
	// true or false — is honored verbatim and never overridden here.
	if _, explicit := m["require_parameters"]; !explicit && hasTools {
		m["require_parameters"] = true
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// maxPriceObject maps an OpenRouterMaxPrice onto OpenRouter's `max_price`
// wire object, emitting each axis ONLY when non-zero: 0 means "no cap on
// this axis" (matching pkg/platform/settings/routingmerge.go's tightenPrice
// semantics), not "cap at $0/M tokens" — emitting a 0 axis would make
// OpenRouter reject every provider on that axis. Returns nil when p is nil or
// both axes are 0 (no cap at all — routingOptions then omits the key).
func maxPriceObject(p *llm.OpenRouterMaxPrice) map[string]any {
	if p == nil {
		return nil
	}
	m := map[string]any{}
	if p.Prompt != 0 {
		m["prompt"] = p.Prompt
	}
	if p.Completion != 0 {
		m["completion"] = p.Completion
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// autoPlugins maps the auto-router hints (AllowedModels/CostQualityTradeoff)
// onto OpenRouter's `plugins` request-body entry; nil when neither is set. The
// hints only apply to Request.Model "openrouter/auto", and OpenRouter ignores
// the plugin for any other model, so emitting it unconditionally is safe.
//
// Wire shape: allowed_models and cost_quality_tradeoff are SIBLINGS of "id",
// not nested under a "config" key:
//
//	"plugins": [{ "id": "auto-router", "allowed_models": ["anthropic/*", "openai/gpt-5.1"], "cost_quality_tradeoff": 3 }]
func autoPlugins(r *llm.OpenRouterRouting) any {
	if len(r.AllowedModels) == 0 && r.CostQualityTradeoff == nil {
		return nil
	}
	plugin := map[string]any{"id": "auto-router"}
	if len(r.AllowedModels) > 0 {
		plugin["allowed_models"] = r.AllowedModels
	}
	if r.CostQualityTradeoff != nil {
		plugin["cost_quality_tradeoff"] = *r.CostQualityTradeoff
	}
	return []any{plugin}
}

// reportedCostUSD extracts OpenRouter's usage.cost extra field from the raw JSON
// of the stream's usage-bearing chunk (openaicompat.RunStream's second return).
// ok=false when no cost was reported — usageRawJSON empty, or present without a
// "cost" key (a non-OpenRouter upstream, or usage.include not honored).
//
// It must NOT read from the accumulated *oa.ChatCompletion: AddChunk never
// populates that Usage field's raw JSON, so cc.Usage.RawJSON() is always "" for
// a streamed response. See openaicompat.RunStream.
func reportedCostUSD(usageRawJSON string) (float64, bool) {
	if usageRawJSON == "" {
		return 0, false
	}
	var raw struct {
		Cost *float64 `json:"cost"`
	}
	if err := json.Unmarshal([]byte(usageRawJSON), &raw); err != nil {
		return 0, false
	}
	if raw.Cost == nil {
		return 0, false
	}
	return *raw.Cost, true
}
