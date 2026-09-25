// Per-chunk translator from the OpenAI SDK's ChatCompletionChunk to
// provider-neutral llm.StreamEvents, so a caller's OnEvent callback never
// depends on the SDK's chunk shape. RunStream invokes it for each chunk.
package openaicompat

import (
	oa "github.com/openai/openai-go/v3"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// StreamState carries cross-chunk bookkeeping for the streaming translator.
// OpenAI correlates tool-call fragments by Index (an int64), and the tool's
// ID/Name arrive only on the first fragment — so we remember which indices
// have already emitted a tool_use_start.
type StreamState struct {
	toolStarted map[int64]bool
}

func NewStreamState() *StreamState {
	return &StreamState{toolStarted: map[int64]bool{}}
}

// TranslateStreamChunk maps a single OpenAI ChatCompletionChunk to zero or more
// neutral llm.StreamEvents. Mirrors anthropic/stream_translate.go's intent:
//   - delta.Content                    → StreamEventTextDelta (BlockIndex 0)
//   - tool-call first fragment (ID/Name) → StreamEventToolUseStart (BlockIndex = tool Index)
//   - tool-call argument fragments     → StreamEventToolUseDeltaArgs
//   - chunk.Usage (final usage chunk)  → StreamEventUsage
//   - choice.FinishReason non-empty    → StreamEventStop (mapped via finishReasonToStop)
//
// Live events are best-effort for progress UI; the authoritative result comes
// from TranslateResponse(&acc.ChatCompletion) after the stream ends.
func TranslateStreamChunk(chunk oa.ChatCompletionChunk, st *StreamState) []llm.StreamEvent {
	var out []llm.StreamEvent

	if len(chunk.Choices) > 0 {
		ch := chunk.Choices[0]
		if ch.Delta.Content != "" {
			out = append(out, llm.StreamEvent{Type: llm.StreamEventTextDelta, BlockIndex: 0, Text: ch.Delta.Content})
		}
		for _, tc := range ch.Delta.ToolCalls {
			idx := tc.Index
			if !st.toolStarted[idx] && (tc.ID != "" || tc.Function.Name != "") {
				st.toolStarted[idx] = true
				out = append(out, llm.StreamEvent{
					Type:       llm.StreamEventToolUseStart,
					BlockIndex: int(idx),
					ToolUseID:  tc.ID,
					ToolName:   tc.Function.Name,
				})
			}
			if tc.Function.Arguments != "" {
				out = append(out, llm.StreamEvent{
					Type:         llm.StreamEventToolUseDeltaArgs,
					BlockIndex:   int(idx),
					JSONFragment: tc.Function.Arguments,
				})
			}
		}
		if ch.FinishReason != "" {
			out = append(out, llm.StreamEvent{Type: llm.StreamEventStop, StopReason: finishReasonToStop(ch.FinishReason)})
		}
	}

	// Usage arrives on a trailing chunk (only when StreamOptions.IncludeUsage
	// is set). CompletionTokens>0 marks the real usage chunk.
	if chunk.Usage.CompletionTokens > 0 {
		out = append(out, llm.StreamEvent{
			Type:  llm.StreamEventUsage,
			Usage: &llm.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens},
		})
	}
	return out
}
