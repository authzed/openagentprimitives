package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryRoundTripFunc adapts a func to http.RoundTripper so a test can inject
// transport-level failures (a connection refused surfaces here as a non-nil
// error with no *http.Response, exactly like a dial to an unbound port).
type retryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f retryRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fastBackoff shrinks the package-level retry backoff to ~nothing for the
// duration of a test so the transient-retry paths run instantly. Tests that
// call it must NOT run t.Parallel — they mutate a package global.
func fastBackoff(t *testing.T) {
	t.Helper()
	orig := memoryHTTPBackoff
	memoryHTTPBackoff = []time.Duration{
		time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond,
		time.Millisecond, time.Millisecond, time.Millisecond,
	}
	t.Cleanup(func() { memoryHTTPBackoff = orig })
}

// A transient connection failure — the operator's :8082 memory endpoint not yet
// listening when a freshly-scheduled session pod fires its first memory read —
// must be ridden out, not fatal. This is the "read memory: ... connect:
// connection refused" regression that ended chat conversations on the first turn.
func TestDo_RetriesTransientTransportError(t *testing.T) {
	fastBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "yes"})
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "tok")
	base := c.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	var calls int32
	c.http.Transport = retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return nil, errors.New("dial tcp 10.43.0.1:8082: connect: connection refused")
		}
		return base.RoundTrip(req)
	})

	var out map[string]string
	err := c.do(context.Background(), http.MethodGet, "/memory/turn/ns/name", nil, &out)
	require.NoError(t, err, "do must ride out a transient connection-refused, not fail the read")
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "2 refused dials + 1 success")
	assert.Equal(t, "yes", out["ok"])
}

// 5xx from a still-starting operator is transient too: retry, then succeed.
func TestDo_Retries5xxThenSucceeds(t *testing.T) {
	fastBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "yes"})
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "tok")
	var out map[string]string
	require.NoError(t, c.do(context.Background(), http.MethodGet, "/x", nil, &out))
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "2x 503 + 1 success")
}

// 4xx is permanent (auth / not-found / bad request): return immediately, never
// retry — retrying an auth failure would just waste the whole backoff budget.
func TestDo_DoesNotRetry4xx(t *testing.T) {
	fastBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "tok")
	err := c.do(context.Background(), http.MethodGet, "/x", nil, nil)
	require.Error(t, err)
	var se *statusError
	require.True(t, errors.As(err, &se), "want a *statusError")
	assert.Equal(t, http.StatusUnauthorized, se.code)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "4xx is permanent — no retry")
}

// The retry loop never sleeps past the caller's context: a persistently-refused
// endpoint gives up when the deadline passes rather than exhausting all attempts.
func TestDo_GivesUpWhenContextExpires(t *testing.T) {
	fastBackoff(t)
	// Nothing listens on port 1, so every dial is refused.
	c := New("http://127.0.0.1:1", "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := c.do(ctx, http.MethodGet, "/x", nil, nil)
	require.Error(t, err, "must give up once the context deadline passes")
}
