package anthropic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/llmhttp"
)

// TestSend_StalledStream_ReturnsStreamIdleError covers a dead stream: the
// server answers 200 with event-stream headers and a message_start, then goes
// silent on a live connection. Send must return an ErrStreamIdle error (which
// the runner routes through its provider-error path) instead of blocking.
func TestSend_StalledStream_ReturnsStreamIdleError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-7\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	c := NewSDKClient("sk-ant-test",
		option.WithBaseURL(srv.URL+"/"),
		option.WithHTTPClient(llmhttp.NewClient(llmhttp.Config{StreamIdleTimeout: 200 * time.Millisecond})),
		option.WithMaxRetries(0),
	)
	p := &Provider{client: &c}

	done := make(chan error, 1)
	go func() {
		_, err := p.Send(t.Context(), llm.Request{
			Model: "claude-opus-4-7", MaxTokens: 100,
			Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}}},
		})
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "a stalled stream must surface as an error")
		assert.ErrorIs(t, err, llmhttp.ErrStreamIdle)
	case <-time.After(10 * time.Second):
		t.Fatal("Send blocked on a stalled stream")
	}
}
