// Package sdkclient is the single place an anthropic-sdk-go client is
// constructed. It is separate from pkg/agent/llm/anthropic because that
// package registers the "anthropic" provider in init(): the secondary-LLM
// helpers (approval summarizer, identity advisor, preview markup) need a
// bounded SDK client, not a registered provider, and importing the provider
// package would register it in every binary that links them.
package sdkclient

import (
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/authzed/openagentprimitives/pkg/agent/llm/llmhttp"
)

// New builds an anthropic-sdk-go client on the shared bounded HTTP client
// (llmhttp.Client: connect/header timeouts, HTTP/2 health pings, and a
// stream-idle abort), so no caller can fall back to the SDK's default client,
// which has no timeouts and lets a silent stream block forever.
//
// Extra opts are appended after the defaults, so a caller's option wins:
// tests point WithBaseURL at a stub and swap in a client with short timeouts.
// Production code must not pass option.WithHTTPClient — that drops the bounds,
// and llmhttp's wiring guard test refuses it.
func New(apiKey string, opts ...option.RequestOption) sdk.Client {
	all := append([]option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(llmhttp.Client()),
	}, opts...)
	return sdk.NewClient(all...)
}
