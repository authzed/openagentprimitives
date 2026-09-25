package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// synthCounter synthesizes a one-tool MCPServer wrapper for a "count" tool at
// url (loopback → plain client, mirroring mustSynthesize).
func synthCounter(t *testing.T, url string) *mcp.MCPTool {
	t.Helper()
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "counter"
	cr.Spec.Server.URL = url
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{{Name: "count", Permission: &authz.Permission{StateImpact: authz.Passthrough}}}
	live := []probe.Tool{{Name: "count"}}
	res, err := mcp.Synthesize(cr, live, mcp.WithHTTPClient(http.DefaultClient))
	require.NoError(t, err, "Synthesize")
	return res.LLMTools[0].(*mcp.MCPTool)
}

// newCounterServer stands up a real go-sdk MCP server whose "count" tool
// increments a counter keyed by the MCP session id and returns "count=N". N only
// climbs across calls that share the same session — so it reveals whether the
// dispatcher reuses one session per AgentSession or opens a fresh one per call.
func newCounterServer(t *testing.T) string {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	srv := mcptest.NewCallServer(mcptest.CallServerOpts{
		Tools: map[string]mcptest.ToolHandler{
			"count": func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
				mu.Lock()
				counts[req.Session.ID()]++
				n := counts[req.Session.ID()]
				mu.Unlock()
				return mcptest.TextResult(fmt.Sprintf("count=%d", n), false), nil
			},
		},
	})
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestExecute_WithSessionCache_ReusesServerSession is the end-to-end wiring test:
// with a SessionCache injected (as the runner does at session start), two Execute
// calls on the same MCPTool ride ONE MCP session, so the server's per-session
// counter climbs 1 → 2. Without the cache the dispatcher opens a fresh session
// per call and the counter is stuck at 1 (the dedicated-mcp bug: selection reset
// every call).
func TestExecute_WithSessionCache_ReusesServerSession(t *testing.T) {
	url := newCounterServer(t)
	mt := synthCounter(t, url)

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })
	mt.SetSessionCache(cache)

	_, opID, sess := newOpAndSess(t)

	res, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "content=%q", res.Content)
	assert.Contains(t, res.Content, "count=1", "first call")

	res, err = mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "content=%q", res.Content)
	assert.Contains(t, res.Content, "count=2", "second call reuses the same MCP session")
}

// TestExecute_WithoutSessionCache_OpensFreshSessionPerCall pins the pre-fix
// behavior as a guard: with no cache wired, each Execute opens its own session,
// so the per-session counter never advances past 1. This is the exact defect the
// SessionCache fixes; keeping it asserted documents why the wiring matters.
func TestExecute_WithoutSessionCache_OpensFreshSessionPerCall(t *testing.T) {
	url := newCounterServer(t)
	mt := synthCounter(t, url)

	_, opID, sess := newOpAndSess(t)

	var last agenttool.Result
	for range 3 {
		var err error
		last, err = mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
		require.NoError(t, err)
		require.False(t, last.IsError, "content=%q", last.Content)
	}
	assert.Contains(t, last.Content, "count=1", "no cache ⇒ fresh session per call ⇒ counter never climbs")
}
