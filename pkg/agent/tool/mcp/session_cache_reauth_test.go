package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// rotatingTokenServer stands up a real go-sdk MCP server that
//
//   - counts tool calls per MCP session id (so a lost session is visible as a
//     counter that restarts at 1, exactly like dedicated-mcp's per-session
//     PermissionSystem selection vanishing), and
//   - 401s any request whose Authorization header is not the CURRENTLY accepted
//     token — which the test rotates mid-run to simulate the operator's
//     proactive pre-expiry OAuth refresh.
//
// rotate returns the number of distinct MCP sessions the server has seen.
func rotatingTokenServer(t *testing.T) (url string, rotate func(newToken string), sessions func() int) {
	t.Helper()

	var mu sync.Mutex
	accepted := ""
	counts := map[string]int{}

	s := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "rotating-token-mcp", Version: "1.0.0"}, nil)
	s.AddTool(&sdkmcp.Tool{Name: "count", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			mu.Lock()
			counts[req.Session.ID()]++
			n := counts[req.Session.ID()]
			mu.Unlock()
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: fmt.Sprintf("count=%d", n)}},
			}, nil
		})
	sdk := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return s }, nil)

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

	return srv.URL, func(newToken string) {
			mu.Lock()
			accepted = newToken
			mu.Unlock()
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(counts)
		}
}

// TestExecute_SessionSurvivesTokenRefresh: an OAuth token rotated mid-session
// keeps ONE MCP session, so server-side per-session state survives.
//
// The reauth path and the session cache disagreed about what identifies a
// session. authTransport reauths INSIDE the live session on a 401 and retries
// there (session.go), so the refreshing call itself keeps its server state — but
// reauthPersist (dispatch.go) also writes the refreshed value back onto the tool,
// so the NEXT Execute presents different bytes for the same credential. A cache
// keyed on those bytes misses, opens a second MCP session, and the server's
// per-session state — the whole reason the cache exists — is silently gone one
// call after a routine token refresh.
func TestExecute_SessionSurvivesTokenRefresh(t *testing.T) {
	const oldToken = "Bearer token-before-refresh"
	const newToken = "Bearer token-after-refresh"

	url, rotate, sessions := rotatingTokenServer(t)
	rotate(oldToken)

	mt := synthCounter(t, url)
	mt.SetAuth("Authorization", oldToken)

	var reauthCalls int
	var mu sync.Mutex
	mt.SetReauth(func(context.Context) (string, string, error) {
		mu.Lock()
		reauthCalls++
		mu.Unlock()
		// The operator refreshed the backing Secret; the broker hands back the
		// same credential's new value.
		return "Authorization", newToken, nil
	})

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })
	mt.SetSessionCache(cache)

	_, opID, sess := newOpAndSess(t)
	call := func(t *testing.T, why string) string {
		t.Helper()
		res, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
		require.NoError(t, err, "Execute (%s)", why)
		require.False(t, res.IsError, "Execute (%s) errored: %s", why, res.Content)
		return res.Content
	}

	assert.Contains(t, call(t, "first call on the original token"), "count=1")
	assert.Contains(t, call(t, "second call, still the original token"), "count=2")

	// The operator's proactive pre-expiry refresh lands: the old token stops
	// being accepted upstream.
	rotate(newToken)

	assert.Contains(t, call(t, "401 → reauth inside the live session"), "count=3",
		"the transport reauths in place, so the refreshing call keeps its server-side state")
	assert.Contains(t, call(t, "the call AFTER the refresh"), "count=4",
		"a refreshed token is the SAME credential: the next call must ride the same MCP session, not open a new one")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, reauthCalls, "one rotation is one re-resolve")
	assert.Equal(t, 1, sessions(), "a token refresh must not fork a second MCP session")
}
