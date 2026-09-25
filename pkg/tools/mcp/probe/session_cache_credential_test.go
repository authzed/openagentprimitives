package probe_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// newAuthRecordingSDKServer stands up a real go-sdk MCP server behind a
// middleware that records the Authorization header of every tools/call POST, so
// a test can assert WHICH credential actually went upstream for each logical
// call — the only thing that matters when two MCPServer CRs share one endpoint.
// It also counts distinct MCP sessions, so session separation can be asserted
// structurally rather than inferred.
func newAuthRecordingSDKServer(t *testing.T) (srv *httptest.Server, callAuths func() []string, sessionCount func() int) {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "auth-echo-mcp", Version: "1.0.0"}, nil)

	var mu sync.Mutex
	seen := map[string]bool{}

	s.AddTool(&mcp.Tool{Name: "echo", Description: "echoes its message", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			mu.Lock()
			seen[req.Session.ID()] = true
			mu.Unlock()
			var args map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &args)
			msg, _ := args["message"].(string)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + msg}}}, nil
		})
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)

	var auths []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := readAndRestoreBody(r)
			require.NoError(t, err)
			if strings.Contains(body, `"tools/call"`) {
				mu.Lock()
				auths = append(auths, r.Header.Get("Authorization"))
				mu.Unlock()
			}
		}
		inner.ServeHTTP(w, r)
	})
	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return srv, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), auths...)
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(seen)
		}
}

// TestSessionCache_DoesNotShareASessionAcrossCredentials is the credential-mixup
// regression test.
//
// Nothing stops two MCPServer CRs from naming the same spec.server.url while
// binding different credentials — there is no uniqueness constraint on the URL
// anywhere (no CRD marker, no admission webhook, no controller check). The runner
// builds ONE SessionCache per AgentSession and wires it into every MCPServer's
// tools, each with its own frozen (header, value). A cache keyed by URL alone
// hands the second CR the session the first CR opened, whose HTTP transport has
// the FIRST CR's credential baked in — so the LLM's dispatch order decides which
// token executes upstream, and a low-privilege, no-approval tool can run carrying
// the high-privilege token. The per-call SpiceDB use_token gate cannot see this:
// it checks the credential the caller INTENDED to send.
func TestSessionCache_DoesNotShareASessionAcrossCredentials(t *testing.T) {
	srv, callAuths, sessionCount := newAuthRecordingSDKServer(t)
	ctx := context.Background()

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	_, err := cache.CallTool(ctx, srv.URL, http.DefaultClient, "echo",
		map[string]any{"message": "a"}, probe.Credential{
			ID: "cred/high", Header: "Authorization", Value: "Bearer token-high",
		})
	require.NoError(t, err, "first call, high-privilege credential")

	_, err = cache.CallTool(ctx, srv.URL, http.DefaultClient, "echo",
		map[string]any{"message": "b"}, probe.Credential{
			ID: "cred/low", Header: "Authorization", Value: "Bearer token-low",
		})
	require.NoError(t, err, "second call, low-privilege credential on the same URL")

	auths := callAuths()
	require.Len(t, auths, 2, "one tools/call POST per logical call")
	assert.Equal(t, "Bearer token-high", auths[0], "the first call carries its own credential")
	assert.Equal(t, "Bearer token-low", auths[1],
		"the second call must carry ITS OWN credential, not the one the first call pinned onto the shared session")
	assert.Equal(t, 2, sessionCount(),
		"two different credentials on one URL are two different principals and must not share one MCP session")
}

// TestSessionCache_SameCredentialSharesOneSession pins the other half: splitting
// by credential must not split by anything else. Two calls on the same credential
// are the same principal and keep sharing one session, so the server-side
// per-session state this cache exists to preserve still survives on an
// AUTHENTICATED server — the unauthenticated case is covered by
// TestSessionCache_ReusesSessionSoServerStatePersists.
func TestSessionCache_SameCredentialSharesOneSession(t *testing.T) {
	srv, callAuths, sessionCount := newAuthRecordingSDKServer(t)
	ctx := context.Background()

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	for _, msg := range []string{"a", "b", "c"} {
		_, err := cache.CallTool(ctx, srv.URL, http.DefaultClient, "echo",
			map[string]any{"message": msg}, probe.Credential{
				ID: "cred/high", Header: "Authorization", Value: "Bearer token-high",
			})
		require.NoError(t, err, "call %q with the one credential", msg)
	}

	assert.Equal(t, []string{"Bearer token-high", "Bearer token-high", "Bearer token-high"}, callAuths())
	assert.Equal(t, 1, sessionCount(), "one credential on one URL is one session, reused")
}

// TestSessionCache_ReusedSessionCarriesTheCallersOwnValue closes the gap the
// identity key opens. Two calls on one credential now share an entry even when
// their values differ, and the entry's transport was built by whoever opened it —
// so a caller that just re-resolved a REVOKED credential (pkg/agent/tool/mcp's
// resolveIfRevoked, which exists so revoked bytes never go upstream again) would
// have the session's superseded value sent on its behalf, defeating the
// re-resolution entirely. The value the caller presents must win on every call.
func TestSessionCache_ReusedSessionCarriesTheCallersOwnValue(t *testing.T) {
	srv, callAuths, sessionCount := newAuthRecordingSDKServer(t)
	ctx := context.Background()

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	for _, tok := range []string{"Bearer token-revoked", "Bearer token-reresolved"} {
		_, err := cache.CallTool(ctx, srv.URL, http.DefaultClient, "echo",
			map[string]any{"message": "m"}, probe.Credential{
				ID: "cred/rotating", Header: "Authorization", Value: tok,
			})
		require.NoError(t, err, "call presenting %q", tok)
	}

	assert.Equal(t, []string{"Bearer token-revoked", "Bearer token-reresolved"}, callAuths(),
		"the second call carries the value IT presented, not the one the session was opened with")
	assert.Equal(t, 1, sessionCount(),
		"one credential is one session: a re-resolved value must not fork a second one")
}
