package probe_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// newProtocolErrorSDKServer stands up a real go-sdk streamable server whose
// `mutate` tool is registered through the LOW-LEVEL Server.AddTool handler and
// returns a Go error AFTER doing its work. The go-sdk turns that into a JSON-RPC
// error RESPONSE (HTTP 200 + an `error` member), which the client surfaces as a
// Go error — indistinguishable, at the `cerr != nil` level, from a dropped
// connection.
//
// invocations counts how many times the tool body ran; toolCallPosts counts how
// many tools/call requests reached the server. A second of either is a
// double-execution of a call the server already dispatched.
func newProtocolErrorSDKServer(t *testing.T) (srv *httptest.Server, invocations, toolCallPosts func() int32) {
	t.Helper()
	var ran, posts atomic.Int32

	s := mcp.NewServer(&mcp.Implementation{Name: "mutating-mcp", Version: "1.0.0"}, nil)
	s.AddTool(&mcp.Tool{Name: "mutate", Description: "mutates state, then fails", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ran.Add(1) // stands in for the side effect: the row is written HERE
			return nil, errors.New("upstream rejected the write")
		})
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := readAndRestoreBody(r)
			require.NoError(t, err)
			if strings.Contains(body, `"tools/call"`) {
				posts.Add(1)
			}
		}
		inner.ServeHTTP(w, r)
	})
	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, ran.Load, posts.Load
}

// TestSessionCache_ProtocolErrorIsNotReIssued is the double-execution guard.
//
// The cached-session retry fires on `cerr != nil` alone, and the go-sdk returns
// a Go error for a JSON-RPC error RESPONSE as well as for a transport failure.
// So a server that mutates state and then answers with a protocol error has its
// tools/call replayed — and the cache is wired to exactly the user-supplied
// sidecar MCP servers (pkg/agent/tool/sidecartoolbox). The server answered; the
// tool ran; re-issuing runs it again.
func TestSessionCache_ProtocolErrorIsNotReIssued(t *testing.T) {
	srv, invocations, toolCallPosts := newProtocolErrorSDKServer(t)

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	_, err := cache.CallTool(context.Background(), srv.URL, http.DefaultClient, "mutate",
		map[string]any{}, probe.Credential{})
	require.Error(t, err, "a JSON-RPC error response must still reach the caller as an error")

	assert.Equal(t, int32(1), toolCallPosts(),
		"the server answered the call; re-issuing it is a second, unrequested mutation")
	assert.Equal(t, int32(1), invocations(),
		"the tool body must run exactly once for one agent-requested tool call")
}

// TestSessionCache_UnknownToolDoesNotTearDownTheSession pins the other half of
// the same branch: a protocol error is not evidence the session died, and this
// cache exists precisely to keep one session alive across calls (a stateful
// server keys its per-request selection by the MCP session id). Dropping and
// re-opening on "unknown tool" silently discards that server-side selection.
func TestSessionCache_UnknownToolDoesNotTearDownTheSession(t *testing.T) {
	srv, sessionCount := newStatefulSDKServer(t)

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })
	ctx := context.Background()

	_, err := cache.CallTool(ctx, srv.URL, http.DefaultClient, "select",
		map[string]any{"value": "adobe"}, probe.Credential{})
	require.NoError(t, err, "first call opens the session")

	_, err = cache.CallTool(ctx, srv.URL, http.DefaultClient, "no_such_tool",
		map[string]any{}, probe.Credential{})
	require.Error(t, err, "an unknown tool is a protocol error and must surface as one")

	_, err = cache.CallTool(ctx, srv.URL, http.DefaultClient, "select",
		map[string]any{"value": "adobe"}, probe.Credential{})
	require.NoError(t, err, "the session must still be usable")

	assert.Equal(t, 1, sessionCount(),
		"an unknown-tool error is not a dropped session; re-opening loses the server-side selection this cache exists to preserve")
}

// readAndRestoreBody reads r.Body for inspection and puts it back so the
// wrapped handler still sees it.
func readAndRestoreBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	return string(b), nil
}
