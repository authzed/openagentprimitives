package markup

import (
	"context"
	"fmt"
	"os"
	"strings"

	oa "github.com/openai/openai-go/v3"

	"github.com/authzed/openagentprimitives/pkg/agent/llm/openaicompat"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/openrouter"
)

// OpenAIModel is the OpenAI model used for markup generation — the OpenAI
// counterpart of AnthropicModel (claude-haiku-4-5).
const OpenAIModel = "gpt-5.4-mini"

// OpenRouterModel is OpenRouter's router alias for the current cheap "mini"
// OpenAI model — an alias (not a pinned slug) so the helper survives
// OpenRouter catalog churn; verified against the live /api/v1/models
// listing.
const OpenRouterModel = "~openai/gpt-mini-latest"

// OpenAIProvider is the OpenAI-Chat-Completions-backed markup generator. One
// round-trip per call. Also backs the "openrouter" NewForProvider case (same
// wire protocol, different endpoint/key/model) via newOpenAICompatible.
type OpenAIProvider struct {
	client oa.Client
	model  string
	name   string
}

// NewOpenAI constructs an OpenAIProvider talking to the default OpenAI
// endpoint. Panics on empty key (matches NewAnthropic). OPENAI_BASE_URL, if
// set, overrides the endpoint (test hook). Routes through
// openaicompat.NewClient like NewOpenRouter — no helper in this package
// hand-rolls its own option.RequestOption slice.
func NewOpenAI(apiKey string) *OpenAIProvider {
	if apiKey == "" {
		panic("markup.NewOpenAI: apiKey must be non-empty")
	}
	client := openaicompat.NewClient(apiKey, os.Getenv("OPENAI_BASE_URL"), nil)
	return newOpenAICompatible(client, OpenAIModel, "openai")
}

// NewOpenRouter constructs an OpenAIProvider that talks to OpenRouter's
// OpenAI-compatible endpoint using an OpenRouter API key. Panics on empty key
// (matches NewOpenAI/NewAnthropic) — routing an OpenRouter key here (rather
// than to NewAnthropic, which would send it to Anthropic's API and fail) is
// the whole point of this constructor.
func NewOpenRouter(apiKey string) *OpenAIProvider {
	if apiKey == "" {
		panic("markup.NewOpenRouter: apiKey must be non-empty")
	}
	client := openaicompat.NewClient(apiKey, openrouter.EffectiveBaseURL(), openrouter.AttributionHeaders())
	return newOpenAICompatible(client, OpenRouterModel, "openrouter")
}

// newOpenAICompatible builds an OpenAIProvider from a pre-built client — the
// shared constructor every OpenAI-wire-compatible caller (NewOpenAI,
// NewOpenRouter) funnels through, so there is exactly one struct literal.
func newOpenAICompatible(client oa.Client, model, name string) *OpenAIProvider {
	return &OpenAIProvider{client: client, model: model, name: name}
}

// Name implements Provider.
func (p *OpenAIProvider) Name() string { return p.name }

// Generate implements Provider. Same contract + security framing as the
// Anthropic impl; only the SDK round-trip differs. The caller sanitizes output.
func (p *OpenAIProvider) Generate(ctx context.Context, instruction string) (string, error) {
	resp, err := p.client.Chat.Completions.New(ctx, oa.ChatCompletionNewParams{
		Model:               oa.ChatModel(p.model),
		MaxCompletionTokens: oa.Int(1024),
		Messages: []oa.ChatCompletionMessageParamUnion{
			oa.SystemMessage(buildSystemPrompt()),
			oa.UserMessage(instruction),
		},
	})
	if err != nil {
		return "", fmt.Errorf("openai.Chat.Completions.New: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("markup: empty response from model")
	}
	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	if raw == "" {
		return "", fmt.Errorf("markup: model produced no text content")
	}
	raw = strings.TrimPrefix(raw, "```html")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("markup: model output was only a markdown fence")
	}
	return raw, nil
}

// NewForProvider returns the markup Provider for the given LLM provider name.
// "openai" → OpenAIProvider against the default OpenAI endpoint; "openrouter"
// → OpenAIProvider against OpenRouter (same wire protocol, different
// endpoint/key/model — an OpenRouter API key sent to NewAnthropic would fail);
// anything else → AnthropicProvider. Switch stays in-package (AGENTS.md).
func NewForProvider(provider, apiKey string) Provider {
	switch provider {
	case "openai":
		return NewOpenAI(apiKey)
	case "openrouter":
		return NewOpenRouter(apiKey)
	default:
		return NewAnthropic(apiKey)
	}
}
