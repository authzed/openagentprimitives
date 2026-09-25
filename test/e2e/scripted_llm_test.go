//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/test/e2e"
)

func TestScriptedLLM_OnUserMessage_FirstRuleMatches(t *testing.T) {
	s := e2e.NewScriptedLLM(t)
	s.OnUserMessage("hello").Reply(e2e.Text("hi back"))

	req := llm.Request{
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{
			{Type: "text", Text: "hello world"},
		}}},
	}
	resp, err := s.Send(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "text", resp.Content[0].Type)
	assert.Equal(t, "hi back", resp.Content[0].Text)
}

func TestScriptedLLM_OnToolResult_MatchesByToolName(t *testing.T) {
	s := e2e.NewScriptedLLM(t)
	s.OnToolResult("list_companies", e2e.AnyResult()).
		Reply(e2e.RespondToUser("found 3"))

	// Build the conversation shape the runner produces: an assistant
	// turn with a tool_use, followed by the user-role tool_result.
	req := llm.Request{
		Messages: []llm.Message{
			{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "list them"}}},
			{Role: "assistant", Content: []llm.ContentBlock{{
				Type:    "tool_use",
				ToolUse: &llm.ToolUseBlock{ID: "toolu_1", Name: "list_companies", Input: json.RawMessage(`{}`)},
			}}},
			{Role: "user", Content: []llm.ContentBlock{{
				Type:       "tool_result",
				ToolResult: &llm.ToolResultBlock{ToolUseID: "toolu_1", Content: `{"items":[]}`},
			}}},
		},
	}
	resp, err := s.Send(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, resp.Content, 2, "RespondToUser = text + tool_use")
	assert.Equal(t, "text", resp.Content[0].Type)
	assert.Equal(t, "tool_use", resp.Content[1].Type)
	require.NotNil(t, resp.Content[1].ToolUse)
	assert.Equal(t, "respond_to_user", resp.Content[1].ToolUse.Name)
}

// TestScriptedLLM_OnToolResult_UnwrapsUntrustedDelimiters verifies that a
// tool_result whose Content the runner wrapped in
// <untrusted-tool-output nonce="…">…</…> delimiters (its prompt-injection
// defence) is still JSON-parsed for the OnToolResult predicate. Without
// unwrapping, json.Unmarshal fails, the predicate receives the raw wrapped
// string instead of the parsed payload, and any rule that chains off a
// tool result (e.g. minting an operation_id) silently stops matching.
func TestScriptedLLM_OnToolResult_UnwrapsUntrustedDelimiters(t *testing.T) {
	s := e2e.NewScriptedLLM(t)

	var gotOperationID string
	s.OnToolResult("new_operation", func(parsed any) bool {
		m, ok := parsed.(map[string]any)
		if !ok {
			return false
		}
		id, _ := m["operation_id"].(string)
		gotOperationID = id
		return id != ""
	}).Reply(e2e.RespondToUser("ok"))

	// The runner wraps tool_result Content exactly like this before it
	// reaches the LLM (pkg/agent/runner wrapUntrustedToolOutput).
	wrapped := "<untrusted-tool-output nonce=\"deadbeefdeadbeef\">\n" +
		`{"operation_id":"op-7"}` + "\n</untrusted-tool-output nonce=\"deadbeefdeadbeef\">"

	req := llm.Request{
		Messages: []llm.Message{
			{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "start"}}},
			{Role: "assistant", Content: []llm.ContentBlock{{
				Type:    "tool_use",
				ToolUse: &llm.ToolUseBlock{ID: "toolu_1", Name: "new_operation", Input: json.RawMessage(`{}`)},
			}}},
			{Role: "user", Content: []llm.ContentBlock{{
				Type:       "tool_result",
				ToolResult: &llm.ToolResultBlock{ToolUseID: "toolu_1", Content: wrapped},
			}}},
		},
	}
	resp, err := s.Send(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "op-7", gotOperationID,
		"predicate must receive the unwrapped, JSON-parsed payload")
	require.Len(t, resp.Content, 2, "rule should have matched → RespondToUser reply")
}

func TestScriptedLLM_UnmatchedRequest_FailsTest(t *testing.T) {
	ft := &fakeT{T: t}
	s := e2e.NewScriptedLLM(ft)
	_, _ = s.Send(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: "user",
			Content: []llm.ContentBlock{{Type: "text", Text: "anything"}}}},
	})
	assert.True(t, ft.fataled, "Send with no rules must Fatalf")
}

func TestScriptedLLM_OnceRuleConsumed(t *testing.T) {
	ft := &fakeT{T: t}
	s := e2e.NewScriptedLLM(ft)
	s.OnUserMessage("hi").Reply(e2e.Text("first"))

	req := llm.Request{Messages: []llm.Message{{Role: "user",
		Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}}}}
	_, err := s.Send(context.Background(), req)
	require.NoError(t, err)
	require.False(t, ft.fataled, "first call must succeed")

	_, _ = s.Send(context.Background(), req)
	assert.True(t, ft.fataled, "consumed rule must not re-match")
}

func TestScriptedLLM_Repeating_MatchesMultipleTimes(t *testing.T) {
	s := e2e.NewScriptedLLM(t)
	s.OnUserMessage("ping").Reply(e2e.Text("pong")).Repeating()

	req := llm.Request{Messages: []llm.Message{{Role: "user",
		Content: []llm.ContentBlock{{Type: "text", Text: "ping"}}}}}
	for i := 0; i < 3; i++ {
		_, err := s.Send(context.Background(), req)
		require.NoError(t, err, "iteration %d", i)
	}
}

func TestScriptedLLM_AssertAllRulesConsumed_Failure(t *testing.T) {
	ft := &fakeT{T: t}
	s := e2e.NewScriptedLLM(ft)
	s.OnUserMessage("never").Reply(e2e.Text("nope"))
	s.AssertAllRulesConsumed()
	assert.True(t, ft.fataled, "unconsumed rule must Fatalf")
}

func TestScriptedLLM_ToolUseInput_IsJSONEncoded(t *testing.T) {
	s := e2e.NewScriptedLLM(t)
	s.OnUserMessage("go").Reply(e2e.ToolUse("foo", map[string]any{"k": "v", "n": 42}))

	resp, err := s.Send(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: "user",
			Content: []llm.ContentBlock{{Type: "text", Text: "go"}}}},
	})
	require.NoError(t, err)
	require.Len(t, resp.Content, 1)
	require.NotNil(t, resp.Content[0].ToolUse)
	assert.Equal(t, "foo", resp.Content[0].ToolUse.Name)
	assert.NotEmpty(t, resp.Content[0].ToolUse.ID)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(resp.Content[0].ToolUse.Input, &decoded))
	assert.Equal(t, "v", decoded["k"])
	assert.Equal(t, float64(42), decoded["n"])
}

func TestScriptedLLM_Refusal_SetsStopReason(t *testing.T) {
	l := e2e.NewScriptedLLM(t)
	l.OnUserMessage("hi").Reply(e2e.Refusal())
	resp, err := l.Send(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	require.NoError(t, err)
	assert.Equal(t, "refusal", resp.StopReason)
	assert.Empty(t, resp.Content)
}

func TestScriptedLLM_Refusal_WithToolUse_KeepsToolButRefuses(t *testing.T) {
	l := e2e.NewScriptedLLM(t)
	l.OnUserMessage("hi").Reply(e2e.ToolUse("artifact_prepare", map[string]any{"kind": "html", "payload": "<h1>x</h1>"}), e2e.Refusal())
	resp, err := l.Send(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	require.NoError(t, err)
	assert.Equal(t, "refusal", resp.StopReason)
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "tool_use", resp.Content[0].Type)
}

func TestScriptedLLM_ReplyErr_ReturnsErrorFromSend(t *testing.T) {
	s := e2e.NewScriptedLLM(t)
	wantErr := errors.New("simulated provider failure")
	s.OnUserMessage("trigger").ReplyErr(wantErr)
	_, err := s.Send(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "trigger"}}}},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

// fakeT captures Fatalf calls for assertion. Embeds *testing.T so it
// satisfies anything that takes a TB but lets us intercept failures.
type fakeT struct {
	*testing.T
	fataled     bool
	lastMessage string
}

func (f *fakeT) Fatalf(format string, args ...any) {
	f.fataled = true
	f.lastMessage = format
}

func (f *fakeT) Helper() {}
