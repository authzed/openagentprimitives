package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
)

// TestSidecarToolRefresher_ToolsShareTheRunnerSessionCache covers the path a
// secret-gated sidecar ACTUALLY arrives on: k8s-mcp is AwaitingSecret at boot
// (its kubeconfig hasn't been produced yet), so its tools are synthesized
// mid-session by the refresher, not the boot pass. A refresher that drops the
// runner's session cache leaves exactly the reported symptom — the agent selects
// a PermissionSystem and the very next call reports none selected.
func TestSidecarToolRefresher_ToolsShareTheRunnerSessionCache(t *testing.T) {
	srv, sessionCount := mcptest.NewSelectionServer()
	t.Cleanup(srv.Close)

	var port int32
	_, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port)
	require.NoError(t, err, "parse port from %q", srv.URL)

	unconstrained := spiceboxv1alpha1.MCPServerToolArgs{UnconstrainedArgs: true}
	ready := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "k8s", Ref: "k8s-mcp", Port: port,
		RunMode: "separate-pod", AwaitingSecret: false, SidecarPodIP: "127.0.0.1",
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name: "k8s",
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "select", Args: unconstrained, Permission: &authz.Permission{StateImpact: authz.Passthrough}},
				{Name: "selected", Args: unconstrained, Permission: &authz.Permission{StateImpact: authz.Passthrough}},
			},
		},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{ready},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(refreshScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	probe := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		return []mcpprobe.Tool{
			{Name: "select", InputSchema: []byte(`{"type":"object"}`)},
			{Name: "selected", InputSchema: []byte(`{"type":"object"}`)},
		}, nil
	}

	cache := mcpprobe.NewSessionCache()
	t.Cleanup(func() { _ = cache.Close() })

	refresh := newSidecarToolRefresher(c, client.ObjectKeyFromObject(sess), probe,
		map[string]string{}, nil, nil, cache, nil)

	res, err := refresh(context.Background())
	require.NoError(t, err, "refresh")
	added := res.Added
	require.Len(t, added, 2, "the ready sidecar's two allowlisted tools are synthesized")

	sel := execRefreshedTool(t, added, "k8s_select", map[string]any{"value": "adobe"})
	require.False(t, sel.IsError, "select failed: %s", sel.Content)

	got := execRefreshedTool(t, added, "k8s_selected", map[string]any{})
	assert.False(t, got.IsError,
		"a mid-session sidecar's tools must ride the runner's session cache; server said: %s", got.Content)
	assert.Contains(t, got.Content, "adobe", "the selection survives to the next call")
	assert.Equal(t, 1, sessionCount(), "both calls ride ONE MCP session")
}

// execRefreshedTool executes one refresher-synthesized tool with the envelope
// the MCP dispatcher expects (operation_id + _reason + args).
func execRefreshedTool(t *testing.T, tools []agenttool.Tool, name string, args map[string]any) agenttool.Result {
	t.Helper()
	var target agenttool.Tool
	for _, tl := range tools {
		if tl.Name() == name {
			target = tl
			break
		}
	}
	require.NotNil(t, target, "tool %q not among refreshed tools", name)

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
