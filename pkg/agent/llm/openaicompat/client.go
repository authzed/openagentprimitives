package openaicompat

import (
	oa "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/authzed/openagentprimitives/pkg/agent/llm/llmhttp"
)

// NewClient builds an OpenAI-compatible client with an optional base-URL
// override and extra headers — the single place "point openai-go at another
// endpoint" lives, and the place every such client gets llmhttp's bounded
// transport. Every OpenAI-wire-compatible provider and secondary-LLM
// helper routes through it, so an OpenRouter-provisioned key cannot fall
// through to the openai default and get sent to the wrong API.
func NewClient(apiKey, baseURL string, headers map[string]string) oa.Client {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		// Bounded transport + stream-idle abort: the SDK's default client has
		// no timeouts, so a silent stream would block the caller forever.
		option.WithHTTPClient(llmhttp.Client()),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	for k, v := range headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return oa.NewClient(opts...)
}
