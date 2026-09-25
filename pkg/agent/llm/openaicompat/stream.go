package openaicompat

import (
	"context"
	"fmt"

	oa "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// RunStream drives one streaming chat-completion turn against an
// OpenAI-compatible endpoint, accumulating the full response and forwarding
// neutral stream events live when onEvent != nil. opts are passed to the
// streaming call — e.g. option.WithJSONSet, which OpenRouter uses to inject
// request-body fields (routing subconfig, usage.include) the neutral
// llm.Request does not model.
//
// Returns the accumulated ChatCompletion plus the raw JSON of the last chunk's
// "usage" object ("" if no chunk carried one). That second return exists
// because oa.ChatCompletionAccumulator does not preserve per-chunk raw JSON:
// after the loop, acc.ChatCompletion.RawJSON() and .Usage.RawJSON() are both
// empty even though the per-chunk Usage.RawJSON() carried extra fields such as
// OpenRouter's usage.cost. A caller needing such a field MUST read it from this
// string; reading it off the returned *oa.ChatCompletion always comes back empty.
func RunStream(ctx context.Context, client oa.Client, params oa.ChatCompletionNewParams, onEvent func(llm.StreamEvent), opts ...option.RequestOption) (*oa.ChatCompletion, string, error) {
	stream := client.Chat.Completions.NewStreaming(ctx, params, opts...)
	defer stream.Close()

	var acc oa.ChatCompletionAccumulator
	var lastUsageRawJSON string
	st := NewStreamState()
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if raw := chunk.Usage.RawJSON(); raw != "" {
			lastUsageRawJSON = raw
		}
		if onEvent != nil {
			for _, ev := range TranslateStreamChunk(chunk, st) {
				onEvent(ev)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, "", fmt.Errorf("openaicompat: chat completions stream: %w", err)
	}
	return &acc.ChatCompletion, lastUsageRawJSON, nil
}
