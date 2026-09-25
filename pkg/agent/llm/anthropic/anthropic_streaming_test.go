// Streaming-flow integration test for Provider.Send.
//
// This test runs the full streaming code path: a stub HTTP server
// returns hand-rolled SSE events, the SDK's ssestream decoder parses
// them, Provider.Send accumulates them and routes neutral
// llm.StreamEvents to OnEvent, and finally returns a translated
// llm.Response.
//
// It lives in `package anthropic` (internal) so it can construct a
// Provider directly with a custom-baseURL SDK client without
// expanding the public API surface.
package anthropic

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// sseScript is the canned event stream the stub server returns. The
// shape mirrors what the real Anthropic API sends for a turn that
// produces a single text block: message_start → content_block_start
// (text) → content_block_delta (text_delta="hello") → content_block_stop
// → message_delta (stop_reason=end_turn, usage.output_tokens=42) →
// message_stop. Each event is `event: <name>\ndata: <json>\n\n`.
const sseScript = `event: message_start
data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-opus-4-7","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

// newStubProvider stands up an httptest server that replies with the
// canned sseScript at /v1/messages, builds an SDK client pointed at
// it, and wires up a Provider. Returns the Provider; cleanup is via
// t.Cleanup.
func newStubProvider(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseScript))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	// Build an SDK client pointed at the stub. WithBaseURL takes
	// precedence over the default; a dummy API key satisfies the
	// SDK's required-header validation.
	c := sdk.NewClient(
		option.WithAPIKey("sk-ant-test"),
		option.WithBaseURL(srv.URL+"/"),
	)
	return &Provider{client: &c}
}

func TestSendStreamingFlow(t *testing.T) {
	p := newStubProvider(t)

	var events []llm.StreamEvent
	req := llm.Request{
		Model:     "claude-opus-4-7",
		MaxTokens: 100,
		Messages: []llm.Message{{
			Role:    "user",
			Content: []llm.ContentBlock{{Type: "text", Text: "say hello"}},
		}},
		OnEvent: func(ev llm.StreamEvent) {
			events = append(events, ev)
		},
	}

	resp, err := p.Send(t.Context(), req)
	require.NoError(t, err, "Send must succeed")

	// Final response.
	assert.Equal(t, "end_turn", resp.StopReason, "StopReason")
	require.Len(t, resp.Content, 1, "Content blocks")
	assert.Equal(t, "text", resp.Content[0].Type, "Content[0].Type")
	assert.Equal(t, "hello", resp.Content[0].Text, "Content[0].Text")
	assert.EqualValues(t, 42, resp.Usage.OutputTokens, "Usage.OutputTokens")

	// Live events.
	var sawTextDelta, sawStop bool
	for _, ev := range events {
		switch ev.Type {
		case llm.StreamEventTextDelta:
			if ev.Text == "hello" {
				sawTextDelta = true
			}
		case llm.StreamEventStop:
			if ev.StopReason == "end_turn" {
				sawStop = true
			}
		}
	}
	assert.True(t, sawTextDelta, "did not see text_delta 'hello' in events: %+v", events)
	assert.True(t, sawStop, "did not see stop end_turn in events: %+v", events)
}

// TestSendStreamingNoOnEvent confirms that a nil OnEvent doesn't
// panic and the final response is still returned correctly. Mirrors
// the path the existing agent loop takes today (pre-streaming
// consumers).
func TestSendStreamingNoOnEvent(t *testing.T) {
	p := newStubProvider(t)

	resp, err := p.Send(t.Context(), llm.Request{
		Model:     "claude-opus-4-7",
		MaxTokens: 100,
		Messages: []llm.Message{{
			Role:    "user",
			Content: []llm.ContentBlock{{Type: "text", Text: "say hello"}},
		}},
	})
	require.NoError(t, err, "Send must succeed with nil OnEvent")
	assert.Equal(t, "end_turn", resp.StopReason, "StopReason")
	require.Len(t, resp.Content, 1, "Content blocks")
	assert.Equal(t, "hello", resp.Content[0].Text, "Content[0].Text")
}
