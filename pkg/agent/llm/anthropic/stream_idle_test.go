package anthropic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic/sdkclient"
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

	c := sdkclient.New("sk-ant-test",
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

// TestSend_HeaderStall_IsRetriedThenFails pins the real worst case for a
// provider that accepts the connection and never answers: the response-header
// timeout is per attempt, and the SDK treats a round trip with no response as
// a retryable connection error, so with its default of two retries the stall
// costs three header timeouts (plus backoff) before Send fails. llmhttp's
// package doc states that bound; this fails if an SDK upgrade or a
// constructor change moves it.
func TestSend_HeaderStall_IsRetriedThenFails(t *testing.T) {
	var attempts atomic.Int32
	// The handler never reads the request body, so the server does not notice
	// the client abandoning the connection; release unblocks it for Close.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		<-release // never sends headers
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close

	c := sdkclient.New("sk-ant-test",
		option.WithBaseURL(srv.URL+"/"),
		option.WithHTTPClient(llmhttp.NewClient(llmhttp.Config{ResponseHeaderTimeout: 100 * time.Millisecond})),
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
		require.Error(t, err, "a server that never answers must surface as an error")
		assert.EqualValues(t, 3, attempts.Load(), "one attempt plus the SDK's two default retries")
	case <-time.After(30 * time.Second):
		t.Fatal("Send blocked on a server that never sent headers")
	}
}
