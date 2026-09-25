package openaicompat

import (
	oa "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// NewClient builds an OpenAI-compatible client with an optional base-URL
// override and extra headers — the single place "point openai-go at another
// endpoint" lives. Every OpenAI-wire-compatible provider and secondary-LLM
// helper routes through it, so an OpenRouter-provisioned key cannot fall
// through to the openai default and get sent to the wrong API.
func NewClient(apiKey, baseURL string, headers map[string]string) oa.Client {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	for k, v := range headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return oa.NewClient(opts...)
}
