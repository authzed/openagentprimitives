package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
)

// TestProbeSynthSidecar_ThreadsSessionCacheToSynthesizedTools pins the wiring at
// the SINGLE shared core every sidecar path goes through — the runner boot pass,
// the mid-session refresher, and the e2e in-process factory. If the cache stops
// reaching the synthesized tools here, every one of those paths silently returns
// to a fresh MCP session per call, which is what made the dedicated-mcp sidecar
// forget its PermissionSystem selection between calls.
func TestProbeSynthSidecar_ThreadsSessionCacheToSynthesizedTools(t *testing.T) {
	srv, sessionCount := mcptest.NewSelectionServer()
	t.Cleanup(srv.Close)

	var port int32
	_, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port)
	require.NoError(t, err, "parse port from %q", srv.URL)

	unconstrained := spiceboxv1alpha1.MCPServerToolArgs{UnconstrainedArgs: true}
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "k8s", Ref: "k8s-mcp", Port: port,
		RunMode: "separate-pod", SidecarPodIP: "127.0.0.1",
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name: "k8s",
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "select", Args: unconstrained, Permission: &authz.Permission{StateImpact: authz.Passthrough}},
				{Name: "selected", Args: unconstrained, Permission: &authz.Permission{StateImpact: authz.Passthrough}},
			},
		},
	}
	prober := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		return []mcpprobe.Tool{
			{Name: "select", InputSchema: []byte(`{"type":"object"}`)},
			{Name: "selected", InputSchema: []byte(`{"type":"object"}`)},
		}, nil
	}

	cache := mcpprobe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	tools, err := runner.ProbeSynthSidecar(context.Background(), srv.URL, rt, prober, nil, nil, nil, cache)
	require.NoError(t, err, "ProbeSynthSidecar")

	sel := execSidecarTool(t, tools, "k8s_select", map[string]any{"value": "adobe"})
	require.False(t, sel.IsError, "select failed: %s", sel.Content)

	got := execSidecarTool(t, tools, "k8s_selected", map[string]any{})
	assert.False(t, got.IsError,
		"the shared core must hand its caller's session cache to the tools it builds; server said: %s", got.Content)
	assert.Contains(t, got.Content, "adobe", "selection made on the first call is visible on the second")
	assert.Equal(t, 1, sessionCount(), "both calls ride ONE MCP session")
}

// execSidecarTool executes one synthesized tool with the envelope the MCP
// dispatcher expects (operation_id + _reason + args).
func execSidecarTool(t *testing.T, tools []agenttool.Tool, name string, args map[string]any) agenttool.Result {
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
