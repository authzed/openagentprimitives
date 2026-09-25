package sidecartoolbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
)

func TestSynthesize_OneToolPerAllowlistedEntry(t *testing.T) {
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "echo",
		Ref:  "echo",
		Port: 18080,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "echo"},
				{Name: "ping"},
			},
		},
	}
	live := []probe.Tool{
		{Name: "echo", InputSchema: []byte(`{"type":"object"}`)},
		{Name: "ping", InputSchema: []byte(`{"type":"object"}`)},
	}
	tools, err := sidecartoolbox.Synthesize(rt, live, nil)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	// Tools follow the existing MCP synthesizer's "<prefix>_<name>" naming —
	// the synthesizer derives the prefix from the synthetic MCPServer's
	// metadata.name (rt.Name).
	wantPrefix := "echo_"
	if got := tools[0].Name(); got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("Tool[0].Name=%q want prefix %q", got, wantPrefix)
	}
}

func TestSynthesize_DriftDetected(t *testing.T) {
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "echo",
		Ref:  "echo",
		Port: 18080,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Tools: []spiceboxv1alpha1.MCPServerTool{{Name: "echo"}},
		},
	}
	live := []probe.Tool{} // server reports no tools
	if _, err := sidecartoolbox.Synthesize(rt, live, nil); err == nil {
		t.Fatal("expected drift error")
	}
}

// TestSynthesize_LoopbackDispatchNotSSRFBlocked verifies the critical fix:
// synthesized sidecar tools use a plain HTTP client (not the SSRF-guarded
// safehttp client), so Execute calls reach the in-pod loopback sidecar
// instead of being rejected with an SSRF error. We prove this by standing
// up a real httptest.Server (which binds 127.0.0.1) and asserting the
// tool call succeeds and the stub was actually reached.
func TestSynthesize_LoopbackDispatchNotSSRFBlocked(t *testing.T) {
	// Real go-sdk MCP server exposing "ping" on loopback (httptest binds
	// 127.0.0.1) so we exercise the same session path as production.
	var reached atomic.Bool
	srv := mcptest.NewCallServer(mcptest.CallServerOpts{
		OnRequest: func(string) { reached.Store(true) },
		Tools: map[string]mcptest.ToolHandler{
			"ping": mcptest.StaticTool(mcptest.TextResult("pong", false)),
		},
	})
	t.Cleanup(srv.Close)

	// Extract the port from the test server's URL; httptest always binds 127.0.0.1.
	var port int32
	if _, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port); err != nil {
		t.Fatalf("could not parse port from %q: %v", srv.URL, err)
	}

	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "echo",
		Ref:  "echo",
		Port: port,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name: "echo",
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "ping", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
			},
		},
	}
	live := []probe.Tool{
		{Name: "ping", InputSchema: []byte(`{"type":"object"}`)},
	}

	tools, err := sidecartoolbox.Synthesize(rt, live, nil)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}

	// Build a minimal SessionContext so the MCP dispatcher's operation_id
	// guard is satisfied.
	reg := operations.New(nil, nil)
	op := reg.Begin("loopback-test")
	sess := &agenttool.SessionContext{Operations: reg}
	envelope, _ := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "loopback test",
		"args":         map[string]any{},
	})

	res, err := tools[0].Execute(context.Background(), json.RawMessage(envelope), sess)
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	// If the SSRF guard were active it would return IsError=true with a
	// rejection message; instead we expect the stub's "pong" response.
	if res.IsError {
		t.Errorf("Execute IsError=true (possible SSRF rejection): content=%q", res.Content)
	}
	if !reached.Load() {
		t.Error("httptest server was never reached — request did not make it to the loopback stub")
	}
}
