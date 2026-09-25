package testing

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFakeServer_HappyPath drives the fake through a REAL go-sdk client session
// (the happy path is now a go-sdk StreamableHTTP server, not a stateless POST).
func TestFakeServer_HappyPath(t *testing.T) {
	srv := New(Behavior{
		Tools: []Tool{{Name: "x", Description: "demo"}},
	})
	defer srv.Close()

	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: srv.Client()}, nil)
	require.NoError(t, err, "connect to fake go-sdk server")
	defer sess.Close()

	res, err := sess.ListTools(ctx, nil)
	require.NoError(t, err, "ListTools")
	require.Len(t, res.Tools, 1)
	assert.Equal(t, "x", res.Tools[0].Name)
}

func TestFakeServer_HeaderRequired(t *testing.T) {
	srv := New(Behavior{
		RequireHeaderName:  "Authorization",
		RequireHeaderValue: "Bearer T",
	})
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
