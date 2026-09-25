package identityadvisor

import (
	"context"
	"fmt"
	"strings"

	oa "github.com/openai/openai-go/v3"
)

// OpenAIModel is the OpenAI model used for identity recommendations when the
// session's provider is "openai" — the OpenAI counterpart of AnthropicModel
// (claude-haiku-4-5).
const OpenAIModel = "gpt-5.4-mini"

// OpenRouterModel is OpenRouter's router alias for the current cheap "mini"
// OpenAI model — an alias (not a pinned slug) so the helper survives
// OpenRouter catalog churn; verified against the live /api/v1/models
// listing.
const OpenRouterModel = "~openai/gpt-mini-latest"

// OpenAICompatibleProvider is the OpenAI-Chat-Completions-backed identity
// advisor. Same contract, prompt, and validation as AnthropicProvider; only
// the SDK round-trip differs. It backs both the "openai" and "openrouter"
// session providers — the caller supplies a pre-built client pointed at the
// right endpoint/key (see pkg/agent/llm/openaicompat.NewClient, and
// pkg/agent/llm/openrouter.EffectiveBaseURL/AttributionHeaders for the
// OpenRouter case) plus the matching model id.
type OpenAICompatibleProvider struct {
	client oa.Client
	model  string
	name   string
}

// NewOpenAICompatible constructs an OpenAICompatibleProvider from a pre-built
// client, model id, and name (used for Name(), e.g. "openai"/"openrouter").
// Panics on empty model (matches NewAnthropic/markup.NewOpenAI/
// markup.NewOpenRouter's panic-on-empty-key convention: those constructors
// take a raw apiKey directly and validate it; this constructor instead takes
// a pre-built client — model is the analogous required, non-defaultable
// input here, and an empty one would silently send every request to no
// specific model). Sending an OpenRouter-provisioned API key to NewAnthropic
// would fail (wrong API); routing "openai"/"openrouter" sessions through this
// constructor instead is the fix.
func NewOpenAICompatible(client oa.Client, model, name string) *OpenAICompatibleProvider {
	if model == "" {
		panic("identityadvisor.NewOpenAICompatible: model must be non-empty")
	}
	return &OpenAICompatibleProvider{client: client, model: model, name: name}
}

// Name implements Provider.
func (p *OpenAICompatibleProvider) Name() string { return p.name }

// Recommend implements Provider. See package doc + threat model. Mirrors
// AnthropicProvider.Recommend's prompt + validation (buildSystemPrompt,
// buildUserPrompt, parseAndValidate are shared, provider-agnostic helpers
// defined in anthropic.go), using Chat Completions as a single non-streaming
// round trip instead of the Anthropic Messages API.
func (p *OpenAICompatibleProvider) Recommend(ctx context.Context, req Request) (Recommendation, error) {
	system := buildSystemPrompt()
	user := buildUserPrompt(req)

	resp, err := p.client.Chat.Completions.New(ctx, oa.ChatCompletionNewParams{
		Model:               oa.ChatModel(p.model),
		MaxCompletionTokens: oa.Int(200),
		Messages: []oa.ChatCompletionMessageParamUnion{
			oa.SystemMessage(system),
			oa.UserMessage(user),
		},
	})
	if err != nil {
		return Recommendation{}, fmt.Errorf("openai.Chat.Completions.New: %w", err)
	}
	if len(resp.Choices) == 0 {
		return Recommendation{}, fmt.Errorf("identityadvisor: empty response from model")
	}
	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	if raw == "" {
		return Recommendation{}, fmt.Errorf("identityadvisor: model produced no text content")
	}

	rec, err := parseAndValidate(raw)
	if err != nil {
		return Recommendation{}, fmt.Errorf("identityadvisor: validate output %q: %w", raw, err)
	}
	return rec, nil
}

// Compile-time interface check.
var _ Provider = (*OpenAICompatibleProvider)(nil)
