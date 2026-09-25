package summarizer

import (
	"context"
	"fmt"
	"os"
	"strings"

	oa "github.com/openai/openai-go/v3"

	"github.com/authzed/openagentprimitives/pkg/agent/llm/openaicompat"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/openrouter"
)

// OpenAIModel is the OpenAI model used for summaries. Cheap, fast, single
// round-trip — the OpenAI counterpart of AnthropicModel (claude-haiku-4-5).
const OpenAIModel = "gpt-5.4-mini"

// OpenRouterModel is OpenRouter's router alias for the current cheap "mini"
// OpenAI model — an alias (not a pinned slug) so the helper survives
// OpenRouter catalog churn; verified against the live /api/v1/models
// listing.
const OpenRouterModel = "~openai/gpt-mini-latest"

// OpenAIProvider is the OpenAI-Chat-Completions-backed summarizer. One
// round-trip per call. No streaming, no tools. Reuses the package's
// provider-agnostic prompt builders, validation, and cache. Also backs the
// "openrouter" NewForProvider case (same wire protocol, different
// endpoint/key/model) via newOpenAICompatible.
type OpenAIProvider struct {
	client oa.Client
	model  string
	name   string
	cache  *Cache
}

// NewOpenAI constructs an OpenAIProvider talking to the default OpenAI
// endpoint. Panics on empty key (matches NewAnthropic / pkg/agent/llm/openai.New
// — a Tier-0 invariant). OPENAI_BASE_URL, if set, overrides the endpoint
// (mirrors the primary provider; also the test hook). Routes through
// openaicompat.NewClient like NewOpenRouter — no helper in this package
// hand-rolls its own option.RequestOption slice.
func NewOpenAI(apiKey string) *OpenAIProvider {
	if apiKey == "" {
		panic("summarizer.NewOpenAI: apiKey must be non-empty")
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
		panic("summarizer.NewOpenRouter: apiKey must be non-empty")
	}
	client := openaicompat.NewClient(apiKey, openrouter.EffectiveBaseURL(), openrouter.AttributionHeaders())
	return newOpenAICompatible(client, OpenRouterModel, "openrouter")
}

// newOpenAICompatible builds an OpenAIProvider from a pre-built client — the
// shared constructor every OpenAI-wire-compatible caller (NewOpenAI,
// NewOpenRouter) funnels through, so there is exactly one struct literal.
func newOpenAICompatible(client oa.Client, model, name string) *OpenAIProvider {
	return &OpenAIProvider{client: client, model: model, name: name, cache: NewCache(1024)}
}

// Name implements Provider.
func (p *OpenAIProvider) Name() string { return p.name }

// Summarize implements Provider. Same contract + security framing as the
// Anthropic impl; only the SDK round-trip differs.
func (p *OpenAIProvider) Summarize(ctx context.Context, req Request) (string, error) {
	if got, ok := p.cache.Get(req); ok {
		return got, nil
	}
	resp, err := p.client.Chat.Completions.New(ctx, oa.ChatCompletionNewParams{
		Model:               oa.ChatModel(p.model),
		MaxCompletionTokens: oa.Int(200),
		Messages: []oa.ChatCompletionMessageParamUnion{
			oa.SystemMessage(buildSystemPrompt()),
			oa.UserMessage(buildUserPrompt(req)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("openai.Chat.Completions.New: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("summarizer: empty response from model")
	}
	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	if raw == "" {
		return "", fmt.Errorf("summarizer: model produced no text content")
	}
	summary, err := parseAndValidate(raw)
	if err != nil {
		return "", fmt.Errorf("summarizer: validate output %q: %w", raw, err)
	}
	p.cache.Put(req, summary)
	return summary, nil
}

// SummarizeAnnotations implements Provider. Same one-round-trip, zero-tools
// discipline as Summarize, but scoped to the annotation boundary: the system
// prompt frames the trusted text as USER DATA, never instructions. No cache —
// AnnotationRequest has no stable hash key (annotation batches are naturally
// per-turn and not expected to repeat verbatim).
func (p *OpenAIProvider) SummarizeAnnotations(ctx context.Context, req AnnotationRequest) (string, error) {
	resp, err := p.client.Chat.Completions.New(ctx, oa.ChatCompletionNewParams{
		Model:               oa.ChatModel(p.model),
		MaxCompletionTokens: oa.Int(200),
		Messages: []oa.ChatCompletionMessageParamUnion{
			oa.SystemMessage(annotationSystemPrompt()),
			oa.UserMessage(clampAnnotationText(req.TrustedText)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("openai.Chat.Completions.New: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("summarizer: empty response from model")
	}
	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	if raw == "" {
		return "", fmt.Errorf("summarizer: model produced no text content")
	}
	summary, err := parseAndValidate(raw)
	if err != nil {
		return "", fmt.Errorf("summarizer: validate output %q: %w", raw, err)
	}
	return summary, nil
}

// NewForProvider returns the summarizer Provider for the given LLM provider
// name. "openai" → OpenAIProvider against the default OpenAI endpoint;
// "openrouter" → OpenAIProvider against OpenRouter (same wire protocol,
// different endpoint/key/model — an OpenRouter API key sent to NewAnthropic
// would fail); anything else (incl. "anthropic" and the empty default) →
// AnthropicProvider. The switch lives here, not in the runner (AGENTS.md:
// consumers don't branch on provider name).
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
