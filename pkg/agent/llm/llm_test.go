package llm_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

func TestStreamEvent_TextDelta_Shape(t *testing.T) {
	e := llm.StreamEvent{
		Type:       llm.StreamEventTextDelta,
		BlockIndex: 0,
		Text:       "hello",
	}
	assert.Equal(t, llm.StreamEventType("text_delta"), e.Type)
	assert.Equal(t, "hello", e.Text)
}

func TestStreamEvent_ToolUseStart_Shape(t *testing.T) {
	e := llm.StreamEvent{
		Type:       llm.StreamEventToolUseStart,
		BlockIndex: 1,
		ToolUseID:  "toolu_abc",
		ToolName:   "linear_list_issues",
	}
	assert.Equal(t, llm.StreamEventType("tool_use_start"), e.Type)
	assert.Equal(t, "linear_list_issues", e.ToolName)
}

func TestRequest_OnEventOptional(t *testing.T) {
	var r llm.Request
	r.OnEvent = func(llm.StreamEvent) {}
	require.NotNil(t, r.OnEvent)
	r.OnEvent = nil // nil is valid
	assert.Nil(t, r.OnEvent)
}
