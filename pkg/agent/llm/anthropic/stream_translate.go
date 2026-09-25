// Per-event translator from the Anthropic SDK's MessageStreamEventUnion to
// provider-neutral llm.StreamEvents, so a caller's OnEvent callback never
// depends on the SDK's union shape.
package anthropic

import (
	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// translateStreamEvent maps a single Anthropic SDK stream event to zero,
// one, or multiple neutral llm.StreamEvents.
//
// Mapping table:
//
//   - content_block_delta / text_delta              → StreamEventTextDelta
//   - content_block_delta / thinking_delta          → StreamEventThinkingDelta
//   - content_block_delta / input_json_delta        → StreamEventToolUseDeltaArgs
//     (no ID; consumers correlate by BlockIndex against the prior tool_use_start)
//   - content_block_start with content_block.type == "tool_use"
//     → StreamEventToolUseStart (carries ToolUseID, ToolName, BlockIndex)
//   - content_block_start with any other type       → empty
//     (we don't surface structural starts; text content arrives via deltas)
//   - content_block_stop                            → StreamEventToolUseStop
//     (BlockIndex only; the consumer correlates against a matching
//     prior tool_use_start)
//   - message_delta with Usage.OutputTokens > 0     → StreamEventUsage
//   - message_delta with non-empty StopReason       → StreamEventStop
//     (when both are present, usage is emitted before stop)
//   - message_start, message_stop, ping, anything unrecognised → empty
func translateStreamEvent(ev sdk.MessageStreamEventUnion) []llm.StreamEvent {
	switch ev.Type {
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			return []llm.StreamEvent{{
				Type:       llm.StreamEventTextDelta,
				BlockIndex: int(ev.Index),
				Text:       ev.Delta.Text,
			}}
		case "thinking_delta":
			return []llm.StreamEvent{{
				Type:       llm.StreamEventThinkingDelta,
				BlockIndex: int(ev.Index),
				Thinking:   ev.Delta.Thinking,
			}}
		case "input_json_delta":
			return []llm.StreamEvent{{
				Type:         llm.StreamEventToolUseDeltaArgs,
				BlockIndex:   int(ev.Index),
				JSONFragment: ev.Delta.PartialJSON,
			}}
		}
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			return []llm.StreamEvent{{
				Type:       llm.StreamEventToolUseStart,
				BlockIndex: int(ev.Index),
				ToolUseID:  ev.ContentBlock.ID,
				ToolName:   ev.ContentBlock.Name,
			}}
		}
	case "content_block_stop":
		return []llm.StreamEvent{{
			Type:       llm.StreamEventToolUseStop,
			BlockIndex: int(ev.Index),
		}}
	case "message_delta":
		var out []llm.StreamEvent
		if ev.Usage.OutputTokens > 0 {
			out = append(out, llm.StreamEvent{
				Type:  llm.StreamEventUsage,
				Usage: &llm.Usage{OutputTokens: ev.Usage.OutputTokens},
			})
		}
		if ev.Delta.StopReason != "" {
			out = append(out, llm.StreamEvent{
				Type:       llm.StreamEventStop,
				StopReason: string(ev.Delta.StopReason),
			})
		}
		return out
	}
	return nil
}
