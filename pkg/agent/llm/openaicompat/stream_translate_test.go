package openaicompat

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	oa "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
)

// helper: build a chunk with a single choice delta.
func textChunk(s string) oa.ChatCompletionChunk {
	var c oa.ChatCompletionChunk
	c.Choices = []oa.ChatCompletionChunkChoice{{Delta: oa.ChatCompletionChunkChoiceDelta{Content: s}}}
	return c
}

func TestTranslateStreamChunk_Text(t *testing.T) {
	st := NewStreamState()
	evs := TranslateStreamChunk(textChunk("hel"), st)
	assert.Len(t, evs, 1)
	assert.Equal(t, llm.StreamEventTextDelta, evs[0].Type)
	assert.Equal(t, "hel", evs[0].Text)
}

func TestTranslateStreamChunk_ToolCallStartThenArgs(t *testing.T) {
	st := NewStreamState()

	// First fragment carries Index+ID+Name → tool_use_start.
	var c1 oa.ChatCompletionChunk
	c1.Choices = []oa.ChatCompletionChunkChoice{{Delta: oa.ChatCompletionChunkChoiceDelta{
		ToolCalls: []oa.ChatCompletionChunkChoiceDeltaToolCall{{
			Index: 0, ID: "call_1",
			Function: oa.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: "get_weather", Arguments: ""},
		}},
	}}}
	start := TranslateStreamChunk(c1, st)
	assert.Equal(t, llm.StreamEventToolUseStart, start[0].Type)
	assert.Equal(t, "call_1", start[0].ToolUseID)
	assert.Equal(t, "get_weather", start[0].ToolName)
	assert.Equal(t, 0, start[0].BlockIndex)

	// Second fragment carries only argument text → tool_use_delta_args.
	var c2 oa.ChatCompletionChunk
	c2.Choices = []oa.ChatCompletionChunkChoice{{Delta: oa.ChatCompletionChunkChoiceDelta{
		ToolCalls: []oa.ChatCompletionChunkChoiceDeltaToolCall{{
			Index:    0,
			Function: oa.ChatCompletionChunkChoiceDeltaToolCallFunction{Arguments: `{"loc":`},
		}},
	}}}
	args := TranslateStreamChunk(c2, st)
	assert.Equal(t, llm.StreamEventToolUseDeltaArgs, args[0].Type)
	assert.Equal(t, `{"loc":`, args[0].JSONFragment)
	assert.Equal(t, 0, args[0].BlockIndex)
}

func TestTranslateStreamChunk_FinishReasonEmitsStop(t *testing.T) {
	st := NewStreamState()
	var c oa.ChatCompletionChunk
	c.Choices = []oa.ChatCompletionChunkChoice{{FinishReason: "tool_calls"}}
	evs := TranslateStreamChunk(c, st)
	assert.Equal(t, llm.StreamEventStop, evs[len(evs)-1].Type)
	assert.Equal(t, "tool_use", evs[len(evs)-1].StopReason)
}
