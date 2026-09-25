package sidecartoolbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
)

// selectionSidecar returns a separate-pod ResolvedSidecarToolbox pointing at
// url (an httptest server on 127.0.0.1), allowlisting NewSelectionServer's two
// tools.
func selectionSidecar(t *testing.T, url string) spiceboxv1alpha1.ResolvedSidecarToolbox {
	t.Helper()
	var port int32
	_, err := fmt.Sscanf(url, "http://127.0.0.1:%d", &port)
	require.NoError(t, err, "parse port from %q", url)
	return spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name:         "k8s",
		Ref:          "k8s-mcp",
		Port:         port,
		RunMode:      "separate-pod",
		SidecarPodIP: "127.0.0.1",
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name: "k8s",
			// unconstrainedArgs mirrors the real dedicated-mcp SidecarToolbox CR,
			// whose tools take free-form args rather than an allowedFields list.
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{
					Name:       "select",
					Args:       spiceboxv1alpha1.MCPServerToolArgs{UnconstrainedArgs: true},
					Permission: &authz.Permission{StateImpact: authz.Passthrough},
				},
				{
					Name:       "selected",
					Args:       spiceboxv1alpha1.MCPServerToolArgs{UnconstrainedArgs: true},
					Permission: &authz.Permission{StateImpact: authz.Passthrough},
				},
			},
		},
	}
}

// selectionLive is the tools/list snapshot matching selectionSidecar's allowlist.
func selectionLive() []probe.Tool {
	return []probe.Tool{
		{Name: "select", InputSchema: []byte(`{"type":"object"}`)},
		{Name: "selected", InputSchema: []byte(`{"type":"object"}`)},
	}
}

// callTool executes one synthesized tool with the envelope the MCP dispatcher
// expects (operation_id + _reason + args).
func callTool(t *testing.T, tools []agenttool.Tool, name string, args map[string]any) agenttool.Result {
	t.Helper()
	var target agenttool.Tool
	for _, tl := range tools {
		if tl.Name() == name {
			target = tl
			break
		}
	}
	require.NotNil(t, target, "tool %q not among synthesized tools", name)

	reg := operations.New(nil, nil)
	op := reg.Begin("session-cache test")
	envelope, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "session-cache test",
		"args":         args,
	})
	require.NoError(t, err, "marshal envelope")

	res, err := target.Execute(context.Background(),
		json.RawMessage(envelope), &agenttool.SessionContext{Operations: reg})
	require.NoError(t, err, "Execute %q", name)
	return res
}

// TestSynthesize_SidecarToolsShareOneMCPSession is the regression test for the
// dedicated-mcp "PS selection isn't persisting across calls" bug. The runner
// owns one probe.SessionCache per AgentSession precisely so a stateful sidecar
// keeps server-side per-session state across tool calls; sidecar-toolbox tools
// were synthesized without it, so every call opened a fresh MCP session and any
// selection made on one call was invisible to the next.
func TestSynthesize_SidecarToolsShareOneMCPSession(t *testing.T) {
	srv, sessionCount := mcptest.NewSelectionServer()
	t.Cleanup(srv.Close)

	cache := probe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	tools, err := sidecartoolbox.Synthesize(selectionSidecar(t, srv.URL), selectionLive(), cache)
	require.NoError(t, err, "Synthesize")

	sel := callTool(t, tools, "k8s_select", map[string]any{"value": "adobe"})
	require.False(t, sel.IsError, "select failed: %s", sel.Content)

	got := callTool(t, tools, "k8s_selected", map[string]any{})
	assert.False(t, got.IsError,
		"selection must survive across tool calls; server said: %s", got.Content)
	assert.Contains(t, got.Content, "adobe",
		"the value selected on the first call must be visible on the second")
	assert.Equal(t, 1, sessionCount(),
		"both calls must ride ONE MCP session (a fresh session per call resets server-side state)")
}

// TestSynthesize_NilSessionCache_OpensSessionPerCall pins the documented
// fallback: with no cache wired (unit tests, any not-yet-wired caller) each call
// opens its own session. This is the behavior that broke dedicated-mcp, kept
// deliberate and visible so a future caller passing nil knows what it buys.
func TestSynthesize_NilSessionCache_OpensSessionPerCall(t *testing.T) {
	srv, sessionCount := mcptest.NewSelectionServer()
	t.Cleanup(srv.Close)

	tools, err := sidecartoolbox.Synthesize(selectionSidecar(t, srv.URL), selectionLive(), nil)
	require.NoError(t, err, "Synthesize")

	sel := callTool(t, tools, "k8s_select", map[string]any{"value": "adobe"})
	require.False(t, sel.IsError, "select failed: %s", sel.Content)

	got := callTool(t, tools, "k8s_selected", map[string]any{})
	assert.True(t, got.IsError, "without a cache the second call lands on a fresh session")
	assert.Equal(t, 2, sessionCount(), "one session per call when no cache is wired")
}
