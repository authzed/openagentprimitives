package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// TestLoop_hasMCPTools_IncludesAppTools pins the SEP-1913 fast-follow: an
// app-ONLY MCP server (whose tools live solely in l.AppTools, never l.Tools)
// must make hasMCPTools() true, so the mcp_trust hook — which validates SEP-1913
// trust annotations and is gated on hasMCPTools() in hooks_dataplane.go — is
// registered for that server's app-tool (proxy-exec) calls. Before the fix
// hasMCPTools ranged l.Tools only, silently skipping trust validation for an
// app-only server.
func TestLoop_hasMCPTools_IncludesAppTools(t *testing.T) {
	// fakeAppTool.Kind() returns tool.KindMCP ("mcp") — see apptoolcall_test.go.
	mcpTool := &fakeAppTool{name: "srv_search"}

	t.Run("app-ONLY MCP server (tool only in l.AppTools) → true", func(t *testing.T) {
		l := &Loop{AppTools: map[string]tool.Tool{"srv_search": mcpTool}}
		assert.True(t, l.hasMCPTools(),
			"an MCP tool present only in the app-visible registry must make hasMCPTools true so mcp_trust registers")
	})

	t.Run("MCP tool in l.Tools → true (LLM path unchanged)", func(t *testing.T) {
		l := &Loop{Tools: []tool.Tool{mcpTool}}
		assert.True(t, l.hasMCPTools())
	})

	t.Run("MCP tools in BOTH registries → true", func(t *testing.T) {
		l := &Loop{Tools: []tool.Tool{mcpTool}, AppTools: map[string]tool.Tool{"srv_search": mcpTool}}
		assert.True(t, l.hasMCPTools())
	})

	t.Run("no MCP tools anywhere → false", func(t *testing.T) {
		l := &Loop{}
		assert.False(t, l.hasMCPTools())
	})
}
