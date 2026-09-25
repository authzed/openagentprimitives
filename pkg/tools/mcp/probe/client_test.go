package probe

import (
	"context"
	"errors"
	"net/http"
	"testing"

	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newClientForBehavior spins up a fake MCP server with the given behavior
// and returns a Client bound to it. The server is closed on test cleanup.
//
// The fake server is an httptest server on 127.0.0.1, which the Client's
// default SSRF-guarded client (correctly) refuses; HTTP is set to the
// loopback-permitting http.DefaultClient so the probe can reach it.
func newClientForBehavior(t *testing.T, b mcptest.Behavior) (*Client, *mcptest.Server) {
	t.Helper()
	srv := mcptest.New(b)
	t.Cleanup(srv.Close)
	return &Client{HTTP: http.DefaultClient, URL: srv.URL}, srv
}

func TestClient_HappyPath(t *testing.T) {
	c, srv := newClientForBehavior(t, mcptest.Behavior{
		Tools: []mcptest.Tool{
			{Name: "search_issues", Description: "find them"},
			{Name: "get_issue"},
		},
	})
	got, err := c.ListTools(context.Background(), "Authorization", "Bearer x")
	require.NoError(t, err, "ListTools")
	require.Len(t, got, 2, "tool count")
	// go-sdk does not guarantee tools/list ordering — assert set membership.
	names := mcptest.ToolNames(got, func(t Tool) string { return t.Name })
	assert.True(t, names["search_issues"] && names["get_issue"], "both tools returned; got %v", names)
	// The auth header is injected on every request in the session, so it is
	// present on LastHeader (which is the session's final request — a DELETE on
	// close — that carries no Content-Type, hence no Content-Type assertion).
	assert.Equal(t, "Bearer x", srv.LastHeader.Get("Authorization"))
}

// TestClient_ListTools_Errors covers the ways ListTools can fail before
// returning a typed *HTTPError: HTTP status codes, malformed JSON, and
// JSON-RPC error envelopes. Each row asserts the returned error's message
// contains a recognizable substring (the typed *HTTPError extraction
// lives in TestClient_HTTPError_TypedAs below).
func TestClient_ListTools_Errors(t *testing.T) {
	cases := []struct {
		name     string
		behavior mcptest.Behavior
		wantSub  string
	}{
		{
			name:     "HTTP 500 surfaces HTTP 500 in error",
			behavior: mcptest.Behavior{Status: 500, RawBody: "boom"},
			wantSub:  "HTTP 500",
		},
		{
			name:     "HTTP 401 surfaces HTTP 401 in error",
			behavior: mcptest.Behavior{Status: 401, RawBody: "unauthorized"},
			wantSub:  "HTTP 401",
		},
		{
			name:     "malformed JSON body surfaces decode in error",
			behavior: mcptest.Behavior{RawBody: "not-json"},
			wantSub:  "decode",
		},
		{
			// go-sdk surfaces a JSON-RPC error response as the error message
			// (here the server-sent "no auth"), wrapped in the connect error.
			name:     "JSON-RPC error envelope surfaces the server message",
			behavior: mcptest.Behavior{JSONRPCError: "no auth"},
			wantSub:  "no auth",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newClientForBehavior(t, tc.behavior)
			_, err := c.ListTools(context.Background(), "", "")
			require.Error(t, err, "ListTools must return error")
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestClient_HTTPError_TypedAs verifies non-2xx responses produce a typed
// *HTTPError that callers can distinguish via errors.As.
func TestClient_HTTPError_TypedAs(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		isAuth  bool
		bodyRaw string
	}{
		{name: "401: typed *HTTPError, IsAuth=true", status: 401, isAuth: true, bodyRaw: "unauthorized"},
		{name: "403: typed *HTTPError, IsAuth=true", status: 403, isAuth: true, bodyRaw: "forbidden"},
		{name: "500: typed *HTTPError, IsAuth=false", status: 500, isAuth: false, bodyRaw: "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newClientForBehavior(t, mcptest.Behavior{Status: tc.status, RawBody: tc.bodyRaw})
			_, err := c.ListTools(context.Background(), "", "")
			require.Error(t, err, "expected error")

			var httpErr *HTTPError
			require.True(t, errors.As(err, &httpErr), "errors.As(*HTTPError) must succeed; err = %v (%T)", err, err)
			assert.Equal(t, tc.status, httpErr.StatusCode)
			assert.Equal(t, tc.isAuth, httpErr.IsAuth())
			assert.Contains(t, httpErr.Body, tc.bodyRaw, "Body should contain raw response")
		})
	}
}

func TestClient_ListTools_SEP1913_Annotations(t *testing.T) {
	t.Run("captures SEP-1913 trust annotations", func(t *testing.T) {
		c, _ := newClientForBehavior(t, mcptest.Behavior{
			Tools: []mcptest.Tool{
				{
					Name: "send_message",
					Annotations: &mcptest.Annotations{
						DestructiveHint:       true,
						MaliciousActivityHint: true,
						Attribution:           []string{"mcp://example/source"},
						InputMetadata:         []byte(`{"destination":["public"],"outcomes":["irreversible"]}`),
						ReturnMetadata:        []byte(`{"source":"internal"}`),
					},
				},
			},
		})
		tools, err := c.ListTools(context.Background(), "", "")
		require.NoError(t, err, "ListTools")
		require.Len(t, tools, 1, "tool count")

		a := tools[0].Annotations
		assert.True(t, a.DestructiveHint, "DestructiveHint should be captured")
		assert.True(t, a.MaliciousActivityHint, "MaliciousActivityHint should be captured")
		assert.Equal(t, []string{"mcp://example/source"}, a.Attribution, "Attribution should be captured")
		assert.NotEmpty(t, a.InputMetadata, "InputMetadata should round-trip as raw JSON")
		assert.NotEmpty(t, a.ReturnMetadata, "ReturnMetadata should round-trip as raw JSON")
	})
}

// The JSON-RPC wire shape (params omission for tools/list, etc.) is now owned by
// the go-sdk client, not this package — the former TestClient_WireShape test
// pinned the hand-rolled client's bytes and no longer applies.
