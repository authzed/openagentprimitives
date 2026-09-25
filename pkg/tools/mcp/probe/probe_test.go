package probe

import (
	"context"
	"net/http"
	"testing"

	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseAnnotations verifies that toolFromSDK populates Tool.Annotations from
// the standard MCP annotation hints a go-sdk server advertises for a tool.
func TestParseAnnotations(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{
		Name:        "foo",
		Annotations: &mcptest.Annotations{DestructiveHint: true, ReadOnlyHint: false, OpenWorldHint: true, Title: "Foo Op"},
	}}})
	t.Cleanup(srv.Close)

	// httptest stub on 127.0.0.1 — inject the loopback-permitting client;
	// the default SSRF-guarded client would (correctly) refuse it.
	c := &Client{HTTP: http.DefaultClient, URL: srv.URL}
	got, err := c.ListTools(context.Background(), "", "")
	require.NoError(t, err, "ListTools")
	require.Len(t, got, 1, "tool count")

	tool := got[0]
	assert.Equal(t, "foo", tool.Name)
	assert.True(t, tool.Annotations.DestructiveHint, "DestructiveHint")
	assert.False(t, tool.Annotations.ReadOnlyHint, "ReadOnlyHint")
	assert.True(t, tool.Annotations.OpenWorldHint, "OpenWorldHint")
	assert.False(t, tool.Annotations.IdempotentHint, "IdempotentHint (omitted, should default to false)")
	assert.Equal(t, "Foo Op", tool.Annotations.Title)
}

// TestParseOutputSchema verifies that toolFromSDK captures the per-tool
// outputSchema (MCP 2025-06-18 revision) a go-sdk server advertises.
func TestParseOutputSchema(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{
		Name:         "foo",
		OutputSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	}}})
	t.Cleanup(srv.Close)

	// httptest stub on 127.0.0.1 — inject the loopback-permitting client;
	// the default SSRF-guarded client would (correctly) refuse it.
	c := &Client{HTTP: http.DefaultClient, URL: srv.URL}
	got, err := c.ListTools(context.Background(), "", "")
	require.NoError(t, err, "ListTools")
	require.Len(t, got, 1, "tool count")

	assert.JSONEq(t, `{"type":"object","properties":{"ok":{"type":"boolean"}}}`, string(got[0].OutputSchema), "OutputSchema raw JSON")
}
