// Package openrouter adapts OpenRouter's OpenAI-compatible Chat Completions API
// to llm.Provider, reusing the shared openaicompat wire translation. It adds
// OpenRouter's dynamic-routing subconfig (models[]/provider{}/plugins[]),
// requests usage accounting (usage.include), and captures the served model +
// reported dollar cost that OpenRouter returns per response.
//
// IMPORTANT: the field/method names below reference
// github.com/openai/openai-go/v3, the same vendored version the openai provider
// uses. Two mechanisms depend on SDK internals — extra-body-field injection
// (option.WithJSONSet) and the mid-stream raw-JSON usage-cost read
// (openaicompat.RunStream) — so re-verify both against the SDK source on an
// upgrade. See extra.go and openaicompat/stream.go.
package openrouter

import (
	"context"
	"os"

	oa "github.com/openai/openai-go/v3"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/models"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/openaicompat"
	"github.com/authzed/openagentprimitives/pkg/x/llmpricing"
)

const (
	envAPIKey  = "OPENROUTER_API_KEY"
	envBaseURL = "OPENROUTER_BASE_URL"

	// DefaultBaseURL is OpenRouter's default Chat Completions endpoint.
	DefaultBaseURL = "https://openrouter.ai/api/v1"
)

// EffectiveBaseURL returns OPENROUTER_BASE_URL when set (test/proxy override),
// else DefaultBaseURL. Every OpenRouter-routed client calls this rather than
// re-reading the env var or duplicating the URL literal.
func EffectiveBaseURL() string {
	if v := os.Getenv(envBaseURL); v != "" {
		return v
	}
	return DefaultBaseURL
}

// AttributionHeaders returns the headers OpenRouter's API convention wants for
// attribution on its leaderboards/logs. One source of truth, shared by this
// Provider and every secondary-LLM helper routed through OpenRouter.
func AttributionHeaders() map[string]string {
	return map[string]string{
		"HTTP-Referer": "https://github.com/authzed/openagentprimitives",
		"X-Title":      "agentprimitives",
	}
}

type Provider struct{ client oa.Client }

// New constructs a Provider with the given API key. Panics on empty key
// (matches openai.New/anthropic.New; the factory path checks empty → error
// before here).
func New(apiKey string) *Provider {
	if apiKey == "" {
		panic("openrouter.New: apiKey must be non-empty")
	}
	return &Provider{client: openaicompat.NewClient(apiKey, EffectiveBaseURL(), AttributionHeaders())}
}

func (*Provider) Name() string { return "openrouter" }

func (*Provider) SupportedFromEnv() bool { return os.Getenv(envAPIKey) != "" }

// Pricing implements llm.Provider — from the shared canonical table. OpenRouter
// routes to many upstream models with independently varying prices, so this is
// a best-effort static estimate for a handful of well-known ids; the
// per-response reported cost (Response.Usage.CostUSD, populated by Send) is
// authoritative and should be preferred by cost-tracking callers when present.
func (*Provider) Pricing(model string) (llm.ModelPricing, bool) {
	p, ok := llmpricing.OpenRouter[model]
	return p, ok
}

// Capabilities implements llm.Provider — from the shared canonical table.
// Unknown model ids (the common case — OpenRouter serves hundreds of models
// this table doesn't enumerate) yield an empty CapabilitySet.
func (*Provider) Capabilities(model string) llm.CapabilitySet {
	return models.OpenRouter[model].Capabilities
}

// NativeInputMIMEs implements llm.Provider — from the shared canonical
// table. No OpenRouter row declares a native MIME set today; every model
// yields the zero-value MIMESet.
func (*Provider) NativeInputMIMEs(model string) llm.MIMESet {
	return models.OpenRouter[model].NativeInputMIMEs
}

// Send drives one chat-completion turn against OpenRouter, always via the
// streaming endpoint. req.ProviderRouting is injected as extra request-body
// fields (models[]/provider{}/plugins[]) alongside usage.include, which is
// always requested so OpenRouter reports its per-response dollar cost.
//
// When req.Tools is non-empty and RequireParameters is unset, provider{} also
// gets require_parameters:true (see providerObject): agents need tools, so
// auto/fallback routing must only pick providers that support the request's
// parameters. An explicit RequireParameters is honored verbatim.
//
// Response.Model is the actually-served upstream model, which may differ from
// req.Model (e.g. "openrouter/auto"); Response.Usage.CostUSD is OpenRouter's
// reported cost when present.
func (p *Provider) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	params, err := openaicompat.BuildParams(req)
	if err != nil {
		return llm.Response{}, err
	}
	cc, usageRawJSON, err := openaicompat.RunStream(ctx, p.client, params, req.OnEvent, routingOptions(req.ProviderRouting, len(req.Tools) > 0)...)
	if err != nil {
		return llm.Response{}, err
	}
	resp, err := openaicompat.TranslateResponse(cc)
	if err != nil {
		return llm.Response{}, err
	}
	if c, ok := reportedCostUSD(usageRawJSON); ok {
		resp.Usage.CostUSD = &c
	}
	return resp, nil
}

// Compile-time interface check.
var _ llm.Provider = (*Provider)(nil)
