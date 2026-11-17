package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	oa "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionResponsesToolRoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/responses", r.URL.Path)
		assert.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, false, body["store"])
		assert.Equal(t, true, body["stream"])
		for _, field := range []string{"max_output_tokens", "temperature", "metadata", "previous_response_id"} {
			assert.NotContains(t, body, field)
		}
		input := body["input"].([]any)
		require.Len(t, input, 3)
		assert.Equal(t, "function_call", input[1].(map[string]any)["type"])
		assert.Equal(t, "oap", input[1].(map[string]any)["namespace"])
		assert.Equal(t, "function_call_output", input[2].(map[string]any)["type"])
		namespace := body["tools"].([]any)[0].(map[string]any)
		assert.Equal(t, "namespace", namespace["type"])
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"Done\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-account-model\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"Done\"}]}],\"usage\":{\"input_tokens\":12,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":4}}}}\n\n")
		require.NoError(t, err)
	}))
	defer server.Close()
	p, err := New("access-token")
	require.NoError(t, err)
	p.client = oa.NewClient(option.WithAPIKey("access-token"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0))
	events := []llm.StreamEvent{}
	response, err := p.Send(context.Background(), llm.Request{Model: "gpt-account-model", MaxTokens: 42, UserID: "private-tag", Tools: []llm.ToolDef{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}, Messages: []llm.Message{
		{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "read it"}}},
		{Role: "assistant", Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "call-1", Name: "read", Input: json.RawMessage(`{}`)}}}},
		{Role: "user", Content: []llm.ContentBlock{{Type: "tool_result", ToolResult: &llm.ToolResultBlock{ToolUseID: "call-1", Content: "result"}}}},
	}, OnEvent: func(e llm.StreamEvent) { events = append(events, e) }})
	require.NoError(t, err)
	assert.Equal(t, "Done", response.Content[0].Text)
	assert.Equal(t, int64(8), response.Usage.InputTokens)
	assert.Equal(t, int64(4), response.Usage.CacheReadTokens)
	assert.Equal(t, llm.StreamEventTextDelta, events[0].Type)
	_, priced := p.Pricing("gpt-account-model")
	assert.False(t, priced)
}

func TestIncompleteAndFailedStreamsAreNeverSuccess(t *testing.T) {
	for _, terminal := range []string{"", "response.failed", "response.incomplete", "error"} {
		t.Run(terminal, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, err := fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
				require.NoError(t, err)
				if terminal != "" {
					_, err = fmt.Fprintf(w, "data: {\"type\":%q}\n\n", terminal)
					require.NoError(t, err)
				}
			}))
			defer server.Close()
			p, err := New("access")
			require.NoError(t, err)
			p.client = oa.NewClient(option.WithAPIKey("access"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0))
			_, err = p.Send(context.Background(), llm.Request{Model: "model"})
			assert.Error(t, err)
		})
	}
}

func TestFinalizedToolCallsSurviveCompletedOutputOmission(t *testing.T) {
	for _, summary := range []string{`[]`, `[{"type":"message","id":"msg-1","content":[{"type":"output_text","text":"Replying"}]}]`} {
		t.Run(summary, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, err := fmt.Fprintf(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"reason-1"}}

data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg-1","content":[{"type":"output_text","text":"Replying"}]}}

data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"respond_to_user","namespace":"oap","arguments":"{\"text\":\"Hello, world!\"}"}}

data: {"type":"response.completed","response":{"model":"gpt-account-model","output":%s,"usage":{"input_tokens":81,"output_tokens":22}}}

`, summary)
				require.NoError(t, err)
			}))
			defer server.Close()
			p, err := New("access")
			require.NoError(t, err)
			p.client = oa.NewClient(option.WithAPIKey("access"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0))
			response, err := p.Send(context.Background(), llm.Request{Model: "model"})
			require.NoError(t, err)
			require.Len(t, response.Content, 2)
			assert.Equal(t, "Replying", response.Content[0].Text)
			require.NotNil(t, response.Content[1].ToolUse)
			assert.Equal(t, "respond_to_user", response.Content[1].ToolUse.Name)
			assert.Equal(t, "call-1", response.Content[1].ToolUse.ID)
			assert.JSONEq(t, `{"text":"Hello, world!"}`, string(response.Content[1].ToolUse.Input))
			assert.Equal(t, "tool_use", response.StopReason)
			assert.Equal(t, int64(22), response.Usage.OutputTokens)
		})
	}
}
