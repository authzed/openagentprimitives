package llmhttp_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm/llmhttp"
)

// newServer starts a test server running h and registers its cleanup.
func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// stallAfterHeaders sends headers and one event, then goes silent until the
// client disconnects — a dead stream on a live connection.
func stallAfterHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "event: ping\ndata: {}\n\n")
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

func TestClient_StreamIdle_AbortsSilentBody(t *testing.T) {
	srv := newServer(t, stallAfterHeaders)
	c := llmhttp.NewClient(llmhttp.Config{StreamIdleTimeout: 200 * time.Millisecond})

	resp, err := c.Get(srv.URL)
	require.NoError(t, err, "headers arrive promptly")
	defer resp.Body.Close()

	start := time.Now()
	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "a silent body must not block forever")
	assert.ErrorIs(t, err, llmhttp.ErrStreamIdle)
	assert.Less(t, time.Since(start), 5*time.Second, "aborted near the idle window")
}

func TestClient_StreamIdle_TrickleKeepsStreamAlive(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Total duration (10×60ms) is well past the idle window, but no single
		// gap is: a stream that keeps talking must never be cut off.
		for range 10 {
			_, _ = io.WriteString(w, "event: ping\ndata: {}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(60 * time.Millisecond)
		}
	})
	c := llmhttp.NewClient(llmhttp.Config{StreamIdleTimeout: 250 * time.Millisecond})

	resp, err := c.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "every gap is under the idle window")
	assert.Contains(t, string(body), "event: ping")
}

// A consumer that is slow between reads (an SDK handing each event to a
// callback that blocks) is not an idle stream: only time spent waiting on the
// provider counts, so a healthy response must survive a pause longer than the
// idle window.
func TestClient_StreamIdle_SlowConsumerBetweenReadsIsNotIdle(t *testing.T) {
	const idle = 150 * time.Millisecond
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		// Well inside the idle window, so the stream itself is never idle.
		time.Sleep(idle / 3)
		_, _ = io.WriteString(w, "second\n")
	})
	c := llmhttp.NewClient(llmhttp.Config{StreamIdleTimeout: idle})

	resp, err := c.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	buf := make([]byte, len("first\n"))
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err, "first chunk")

	time.Sleep(3 * idle) // the consumer, not the provider, is slow

	rest, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "time between reads must not count as stream idleness")
	assert.Equal(t, "second\n", string(rest))
}

func TestClient_ResponseHeaderTimeout_AbortsSilentServer(t *testing.T) {
	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never sends headers
	})
	c := llmhttp.NewClient(llmhttp.Config{ResponseHeaderTimeout: 200 * time.Millisecond})

	start := time.Now()
	_, err := c.Get(srv.URL) //nolint:bodyclose // errors before a body exists
	require.Error(t, err, "a server that never answers must not block forever")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestClient_SharedClientIsSingleton(t *testing.T) {
	assert.Same(t, llmhttp.Client(), llmhttp.Client(), "one pool per process")
}
