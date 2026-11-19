package openaicompat

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	oa "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm/llmhttp"
)

// TestRunStream_StalledStream_ReturnsStreamIdleError covers the openai and
// openrouter providers (both drive RunStream): a stream that sends one chunk
// and then goes silent must error with ErrStreamIdle, not block forever.
func TestRunStream_StalledStream_ReturnsStreamIdleError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	client := NewClient("sk-test", srv.URL, nil)
	params := oa.ChatCompletionNewParams{
		Model:    "m",
		Messages: []oa.ChatCompletionMessageParamUnion{oa.UserMessage("hi")},
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := RunStream(t.Context(), client, params, nil,
			option.WithHTTPClient(llmhttp.NewClient(llmhttp.Config{StreamIdleTimeout: 200 * time.Millisecond})),
			option.WithMaxRetries(0))
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "a stalled stream must surface as an error")
		assert.ErrorIs(t, err, llmhttp.ErrStreamIdle)
	case <-time.After(10 * time.Second):
		t.Fatal("RunStream blocked on a stalled stream")
	}
}

// TestRunStream_HeaderStall_IsRetriedThenFails pins the real worst case for a
// provider that accepts the connection and never answers: the response-header
// timeout is per attempt, and the SDK treats a round trip with no response as
// a retryable connection error, so with its default of two retries the stall
// costs three header timeouts (plus backoff) before RunStream fails. llmhttp's
// package doc states that bound; this fails if an SDK upgrade or a
// constructor change moves it.
func TestRunStream_HeaderStall_IsRetriedThenFails(t *testing.T) {
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

	client := NewClient("sk-test", srv.URL, nil)
	params := oa.ChatCompletionNewParams{
		Model:    "m",
		Messages: []oa.ChatCompletionMessageParamUnion{oa.UserMessage("hi")},
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := RunStream(t.Context(), client, params, nil,
			option.WithHTTPClient(llmhttp.NewClient(llmhttp.Config{ResponseHeaderTimeout: 100 * time.Millisecond})))
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "a server that never answers must surface as an error")
		assert.EqualValues(t, 3, attempts.Load(), "one attempt plus the SDK's two default retries")
	case <-time.After(30 * time.Second):
		t.Fatal("RunStream blocked on a server that never sent headers")
	}
}
