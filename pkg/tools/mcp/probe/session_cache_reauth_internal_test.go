package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRotatingTokenServer is a go-sdk MCP server that counts calls per MCP
// session id and 401s anything not carrying the currently-accepted token, so a
// test can rotate the credential mid-run the way the operator's proactive
// pre-expiry OAuth refresh does.
func newRotatingTokenServer(t *testing.T) (url string, rotate func(string), sessions func() int) {
	t.Helper()

	var mu sync.Mutex
	accepted := ""
	counts := map[string]int{}

	s := mcp.NewServer(&mcp.Implementation{Name: "rotating-token-mcp", Version: "1.0.0"}, nil)
	s.AddTool(&mcp.Tool{Name: "count", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			mu.Lock()
			counts[req.Session.ID()]++
			n := counts[req.Session.ID()]
			mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("count=%d", n)}}}, nil
		})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		want := accepted
		mu.Unlock()
		if r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sdk.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv.URL, func(tok string) {
			mu.Lock()
			accepted = tok
			mu.Unlock()
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(counts)
		}
}

// TestSessionCache_TokenRefreshKeepsOneEntryAndOneSession: rotating a
// credential's VALUE must not change which cache entry it belongs to.
//
// The credential identity a caller passes is stable across rotation; the bytes
// are not. Keying on the bytes made every refresh a cache miss — a second MCP
// session (server-side per-session state gone) plus a live orphan entry for the
// superseded value that nothing reclaimed until Close. Both are asserted here:
// one session at the server, one entry in byKey.
func TestSessionCache_TokenRefreshKeepsOneEntryAndOneSession(t *testing.T) {
	const oldToken = "Bearer token-before-refresh"
	const newToken = "Bearer token-after-refresh"

	url, rotate, sessions := newRotatingTokenServer(t)
	rotate(oldToken)

	c := NewSessionCache()
	t.Cleanup(func() { _ = c.Close() })

	// The caller re-resolves the same credential source on a 401 and, like
	// dispatch's reauthPersist, remembers the new value for its next call.
	current := oldToken
	var mu sync.Mutex
	reauth := func(context.Context) (string, string, error) {
		mu.Lock()
		current = newToken
		mu.Unlock()
		return "Authorization", newToken, nil
	}
	call := func(t *testing.T, why string) string {
		t.Helper()
		mu.Lock()
		v := current
		mu.Unlock()
		out, err := c.CallTool(context.Background(), url, http.DefaultClient, "count",
			map[string]any{}, Credential{
				ID: "cred/stable-credential-id", Header: "Authorization", Value: v, Reauth: reauth,
			})
		require.NoError(t, err, "CallTool (%s)", why)
		require.Len(t, out.Content, 1)
		return out.Content[0].Text
	}

	assert.Equal(t, "count=1", call(t, "first call on the original token"))
	rotate(newToken)
	assert.Equal(t, "count=2", call(t, "401 → reauth inside the live session"))
	assert.Equal(t, "count=3", call(t, "the call AFTER the refresh"))

	assert.Equal(t, 1, sessions(), "a refreshed token is the same credential: one MCP session")

	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Len(t, c.byKey, 1,
		"a rotation must re-use the credential's entry, not strand a live one under the superseded value")
}
