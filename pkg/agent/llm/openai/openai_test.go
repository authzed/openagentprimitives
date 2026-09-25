package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sseServer starts an httptest server that emits the given JSON chunk bodies
// as an SSE stream (`data: {json}\n\n` per chunk, terminated by
// `data: [DONE]\n\n`). The openai-go SDK's streaming client (used
// unconditionally by Send as of Phase B) requires an SSE
// `text/event-stream` body — a single plain-JSON body (the old
// non-streaming fixture shape) is NOT parsed into any chunk, so
// openaicompat.TranslateResponse sees zero choices and errors. Verified empirically:
// running the old plain-JSON fixtures against the now-streaming Send
// produced "openaicompat: response had no choices" for both TestSend_ToolCallResponse
// and TestSend_TextOnlyResponse.
func sseServer(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSend_ToolCallResponse(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"role":"assistant","content":"checking"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"loc\":\"NYC\"}"}}]}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
	}
	srv := sseServer(t, chunks)
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	p := New("sk-test")
	resp, err := p.Send(context.Background(), llm.Request{
		Model:    "gpt-5.3-codex",
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "weather?"}}}},
	})
	require.NoError(t, err)

	assert.Equal(t, "tool_use", resp.StopReason)
	require.True(t, resp.HasToolUses())
	uses := resp.ToolUses()
	require.Len(t, uses, 1)
	assert.Equal(t, "call_9", uses[0].ID)
	assert.Equal(t, "get_weather", uses[0].Name)
	assert.JSONEq(t, `{"loc":"NYC"}`, string(uses[0].Input))
	assert.EqualValues(t, 11, resp.Usage.InputTokens)
	assert.EqualValues(t, 7, resp.Usage.OutputTokens)
}

// TestSend_OnEventForwardsLiveDeltas asserts Send forwards neutral
// StreamEvents through req.OnEvent while streaming, on top of returning the
// same final Response as TestSend_ToolCallResponse.
func TestSend_OnEventForwardsLiveDeltas(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"role":"assistant","content":"checking"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"loc\":\"NYC\"}"}}]}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
	}
	srv := sseServer(t, chunks)
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	p := New("sk-test")
	var events []llm.StreamEvent
	resp, err := p.Send(context.Background(), llm.Request{
		Model:    "gpt-5.3-codex",
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "weather?"}}}},
		OnEvent:  func(ev llm.StreamEvent) { events = append(events, ev) },
	})
	require.NoError(t, err)

	// Same final Response as the non-live-events test.
	assert.Equal(t, "tool_use", resp.StopReason)
	require.True(t, resp.HasToolUses())

	// Live events observed, in order: text delta, tool_use_start, tool_use
	// args delta, stop, usage.
	require.NotEmpty(t, events)
	assert.Equal(t, llm.StreamEventTextDelta, events[0].Type)
	assert.Equal(t, "checking", events[0].Text)

	var sawStart, sawArgs, sawStop, sawUsage bool
	for _, ev := range events {
		switch ev.Type {
		case llm.StreamEventToolUseStart:
			sawStart = true
			assert.Equal(t, "call_9", ev.ToolUseID)
			assert.Equal(t, "get_weather", ev.ToolName)
		case llm.StreamEventToolUseDeltaArgs:
			sawArgs = true
			assert.Equal(t, `{"loc":"NYC"}`, ev.JSONFragment)
		case llm.StreamEventStop:
			sawStop = true
			assert.Equal(t, "tool_use", ev.StopReason)
		case llm.StreamEventUsage:
			sawUsage = true
			require.NotNil(t, ev.Usage)
			assert.EqualValues(t, 11, ev.Usage.InputTokens)
			assert.EqualValues(t, 7, ev.Usage.OutputTokens)
		}
	}
	assert.True(t, sawStart, "expected a tool_use_start event")
	assert.True(t, sawArgs, "expected a tool_use_delta_args event")
	assert.True(t, sawStop, "expected a stop event")
	assert.True(t, sawUsage, "expected a usage event")
}

func TestSend_TextOnlyResponse(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{"role":"assistant","content":"hello there"}}]}`,
		`{"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
	}
	srv := sseServer(t, chunks)
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	p := New("sk-test")
	resp, err := p.Send(context.Background(), llm.Request{
		Model:    "gpt-5.3-codex",
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	require.NoError(t, err)

	assert.Equal(t, "end_turn", resp.StopReason)
	assert.False(t, resp.HasToolUses())
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "text", resp.Content[0].Type)
	assert.Equal(t, "hello there", resp.Content[0].Text)
}

func TestSend_NoChoicesIsError(t *testing.T) {
	// A stream whose only chunk is the trailing usage chunk (Choices always
	// empty) — the accumulator ends up with zero Choices, same as a
	// non-streaming response with an empty choices array.
	chunks := []string{
		`{"id":"chatcmpl-3","object":"chat.completion.chunk","created":1,"model":"gpt-5.3-codex","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`,
	}
	srv := sseServer(t, chunks)
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	p := New("sk-test")
	_, err := p.Send(context.Background(), llm.Request{
		Model:    "gpt-5.3-codex",
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	require.Error(t, err) // empty Choices must not yield a zero-value Response silently.
}
