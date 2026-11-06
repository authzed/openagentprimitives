package openaicompat

import (
	"io"
	"net/http"
	"net/http/httptest"
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
