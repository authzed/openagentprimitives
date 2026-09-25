package fake_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
)

func TestProviderScripted(t *testing.T) {
	resp1 := llm.Response{
		Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu_1", Name: "agent_complete", Input: json.RawMessage(`{"summary":"ok"}`)}},
		},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}
	p := fake.New([]fake.Step{{Resp: resp1}})

	got, err := p.Send(context.Background(), llm.Request{Model: "test"})
	require.NoError(t, err)
	require.True(t, got.HasToolUses(), "expected tool use in response")
	uses := got.ToolUses()
	require.NotEmpty(t, uses)
	assert.Equal(t, "agent_complete", uses[0].Name)
}

func TestProviderRunsOutOfScript(t *testing.T) {
	p := fake.New(nil)
	_, err := p.Send(context.Background(), llm.Request{})
	require.Error(t, err, "expected error when script is exhausted")
}

func TestProviderReturnsCannedError(t *testing.T) {
	p := fake.New([]fake.Step{{Err: fake.ErrInjected}})
	_, err := p.Send(context.Background(), llm.Request{})
	assert.ErrorIs(t, err, fake.ErrInjected)
}
