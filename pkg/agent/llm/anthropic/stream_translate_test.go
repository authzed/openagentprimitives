// Tests for translateStreamEvent — the per-event mapper from
// Anthropic SDK MessageStreamEventUnion to neutral llm.StreamEvents.
//
// Tests live in `package anthropic` (internal) so they can call the
// unexported translator directly without exposing it on the public
// API surface.
package anthropic

import (
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// TestTranslateStreamEvent runs the per-event mapper across every
// translation case we care about. Each row supplies one SDK event
// and the expected neutral []llm.StreamEvent fan-out.
func TestTranslateStreamEvent(t *testing.T) {
	cases := []struct {
		name string
		ev   sdk.MessageStreamEventUnion
		want []llm.StreamEvent
	}{
		{
			name: "text_delta content_block_delta: emits StreamEventTextDelta with text + index",
			ev: sdk.MessageStreamEventUnion{
				Type:  "content_block_delta",
				Index: 3,
				Delta: sdk.MessageStreamEventUnionDelta{
					Type: "text_delta",
					Text: "hello world",
				},
			},
			want: []llm.StreamEvent{{
				Type:       llm.StreamEventTextDelta,
				BlockIndex: 3,
				Text:       "hello world",
			}},
		},
		{
			name: "thinking_delta content_block_delta: emits StreamEventThinkingDelta with thinking + index",
			ev: sdk.MessageStreamEventUnion{
				Type:  "content_block_delta",
				Index: 0,
				Delta: sdk.MessageStreamEventUnionDelta{
					Type:     "thinking_delta",
					Thinking: "let me think",
				},
			},
			want: []llm.StreamEvent{{
				Type:       llm.StreamEventThinkingDelta,
				BlockIndex: 0,
				Thinking:   "let me think",
			}},
		},
		{
			name: "input_json_delta content_block_delta: emits StreamEventToolUseDeltaArgs with JSON fragment",
			ev: sdk.MessageStreamEventUnion{
				Type:  "content_block_delta",
				Index: 2,
				Delta: sdk.MessageStreamEventUnionDelta{
					Type:        "input_json_delta",
					PartialJSON: `{"name":"x"`,
				},
			},
			want: []llm.StreamEvent{{
				Type:         llm.StreamEventToolUseDeltaArgs,
				BlockIndex:   2,
				JSONFragment: `{"name":"x"`,
			}},
		},
		{
			name: "tool_use content_block_start: emits StreamEventToolUseStart with id+name+index",
			ev: sdk.MessageStreamEventUnion{
				Type:  "content_block_start",
				Index: 1,
				ContentBlock: sdk.ContentBlockStartEventContentBlockUnion{
					Type: "tool_use",
					ID:   "toolu_abc",
					Name: "read_file",
				},
			},
			want: []llm.StreamEvent{{
				Type:       llm.StreamEventToolUseStart,
				BlockIndex: 1,
				ToolUseID:  "toolu_abc",
				ToolName:   "read_file",
			}},
		},
		{
			name: "text content_block_start: no neutral payload (text arrives via deltas)",
			ev: sdk.MessageStreamEventUnion{
				Type:  "content_block_start",
				Index: 0,
				ContentBlock: sdk.ContentBlockStartEventContentBlockUnion{
					Type: "text",
				},
			},
			want: nil,
		},
		{
			name: "content_block_stop: emits StreamEventToolUseStop with index (consumer correlates against any prior tool_use_start)",
			ev: sdk.MessageStreamEventUnion{
				Type:  "content_block_stop",
				Index: 5,
			},
			want: []llm.StreamEvent{{
				Type:       llm.StreamEventToolUseStop,
				BlockIndex: 5,
			}},
		},
		{
			name: "message_delta with usage + stop_reason: emits StreamEventUsage then StreamEventStop in order",
			ev: sdk.MessageStreamEventUnion{
				Type: "message_delta",
				Delta: sdk.MessageStreamEventUnionDelta{
					StopReason: "end_turn",
				},
				Usage: sdk.MessageDeltaUsage{
					OutputTokens: 42,
				},
			},
			want: []llm.StreamEvent{
				{
					Type:  llm.StreamEventUsage,
					Usage: &llm.Usage{OutputTokens: 42},
				},
				{
					Type:       llm.StreamEventStop,
					StopReason: "end_turn",
				},
			},
		},
		{
			name: "message_delta with stop_reason only: emits StreamEventStop with tool_use reason",
			ev: sdk.MessageStreamEventUnion{
				Type: "message_delta",
				Delta: sdk.MessageStreamEventUnionDelta{
					StopReason: "tool_use",
				},
			},
			want: []llm.StreamEvent{{
				Type:       llm.StreamEventStop,
				StopReason: "tool_use",
			}},
		},
		{
			name: "message_start: not surfaced (no neutral payload)",
			ev: sdk.MessageStreamEventUnion{
				Type: "message_start",
			},
			want: nil,
		},
		{
			name: "ping: not surfaced (SDK strips these but translator stays defensive)",
			ev: sdk.MessageStreamEventUnion{
				Type: "ping",
			},
			want: nil,
		},
		{
			name: "signature_delta content_block_delta: not surfaced (no neutral payload)",
			ev: sdk.MessageStreamEventUnion{
				Type:  "content_block_delta",
				Index: 0,
				Delta: sdk.MessageStreamEventUnionDelta{
					Type:      "signature_delta",
					Signature: "sig",
				},
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateStreamEvent(tc.ev)
			require.Len(t, got, len(tc.want), "event count mismatch: got %+v", got)
			for i := range tc.want {
				// Usage is a pointer; compare via dereference where set.
				if tc.want[i].Usage != nil {
					require.NotNil(t, got[i].Usage, "got[%d].Usage must not be nil", i)
					assert.Equal(t, *tc.want[i].Usage, *got[i].Usage, "got[%d].Usage", i)
					// Compare the rest without the pointer field.
					gotCopy := got[i]
					wantCopy := tc.want[i]
					gotCopy.Usage = nil
					wantCopy.Usage = nil
					assert.Equal(t, wantCopy, gotCopy, "got[%d] (non-Usage fields)", i)
				} else {
					assert.Equal(t, tc.want[i], got[i], "got[%d]", i)
				}
			}
		})
	}
}
