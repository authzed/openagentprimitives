// Package openai adapts the official OpenAI Go SDK (Chat Completions) to the
// llm.Provider interface. Neutral tool_use/tool_result blocks are translated
// to/from OpenAI tool_calls; the portable Cacheable hint is a no-op (OpenAI's
// cache economics are not modeled — see pkg/agent/llm/models.WithoutCacheRatios).
//
// The API key is a static string held in the SDK client. OPENAI_BASE_URL
// overrides the base URL — set explicitly (the SDK reads it too) so a
// Codex/subscription endpoint can be pointed at. Send always uses the SDK's
// streaming endpoint and accumulates the full response; Request.OnEvent, when
// set, receives live neutral StreamEvents.
//
// IMPORTANT: the field names below reference github.com/openai/openai-go/v3.
// If the SDK is upgraded, re-verify them with `go doc`.
package openai

import (
	"context"
	"fmt"
	"os"

	oa "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/models"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/openaicompat"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/providers"
	"github.com/authzed/openagentprimitives/pkg/x/llmpricing"
)

const envAPIKey = "OPENAI_API_KEY"
const envBaseURL = "OPENAI_BASE_URL"

type Provider struct {
	client oa.Client
}

// New constructs a Provider with the given API key. Panics on empty key
// (matches anthropic.New; the factory path checks empty → error before here).
// OPENAI_BASE_URL, if set, overrides the default endpoint (for a Codex /
// subscription base URL).
func New(apiKey string) *Provider {
	if apiKey == "" {
		panic("openai.New: apiKey must be non-empty")
	}
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if base := os.Getenv(envBaseURL); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	return &Provider{client: oa.NewClient(opts...)}
}

func (*Provider) Name() string { return "openai" }

func (*Provider) SupportedFromEnv() bool { return os.Getenv(envAPIKey) != "" }

// Pricing implements llm.Provider — provider-owned, from the shared canonical
// table so runner cost and the admin dashboard can't drift. Unknown model ⇒ ok=false.
func (*Provider) Pricing(model string) (llm.ModelPricing, bool) {
	p, ok := llmpricing.OpenAI[model]
	return p, ok
}

// Capabilities implements llm.Provider — from the shared canonical table.
// Unknown/OpenAI models yield an empty set today (no OpenAI model advertises
// a capability flag).
func (*Provider) Capabilities(model string) llm.CapabilitySet {
	return models.OpenAI[model].Capabilities
}

// NativeInputMIMEs implements llm.Provider — from the shared canonical
// table. No OpenAI row declares a native MIME set today; every model yields
// the zero-value MIMESet.
func (*Provider) NativeInputMIMEs(model string) llm.MIMESet {
	return models.OpenAI[model].NativeInputMIMEs
}

// Send drives one chat-completion turn. It always uses the SDK's streaming
// endpoint and accumulates the full response; when req.OnEvent is set, neutral
// stream events are forwarded live. The final llm.Response is identical to the
// non-streaming result (rebuilt from the accumulator).
func (p *Provider) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	params, err := openaicompat.BuildParams(req)
	if err != nil {
		return llm.Response{}, err
	}
	cc, _, err := openaicompat.RunStream(ctx, p.client, params, req.OnEvent)
	if err != nil {
		return llm.Response{}, err
	}
	return openaicompat.TranslateResponse(cc)
}

func init() {
	providers.Register("openai", func(apiKey string) (llm.Provider, error) {
		if apiKey == "" {
			return nil, fmt.Errorf("openai: apiKey must be non-empty")
		}
		return New(apiKey), nil
	})
}

// Compile-time interface check.
var _ llm.Provider = (*Provider)(nil)
