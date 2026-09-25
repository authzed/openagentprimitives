package runner

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/stretchr/testify/assert"
)

// TestBuildToolDefs_ExcludesAppTools is the CRITICAL security invariant for
// Task 4 of the MCP-UI app-tools split: l.AppTools is a structurally
// separate registry from l.Tools, and buildToolDefs — the sole builder of
// the LLM-facing tool-def list sent to the provider — must iterate l.Tools
// ONLY. An app-visible-only MCP tool (populated into AppTools by
// mcp.Synthesize's split) must never appear in the model's tool list,
// regardless of what else is wired onto the Loop.
func TestBuildToolDefs_ExcludesAppTools(t *testing.T) {
	l := &Loop{
		Tools: []tool.Tool{&fakeMCPTool{name: "model_tool"}},
		AppTools: map[string]tool.Tool{
			"app_tool": &fakeMCPTool{name: "app_tool"},
		},
	}

	defs := l.buildToolDefs()

	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}

	assert.Contains(t, names, "model_tool", "the LLM-visible tool must appear in buildToolDefs' output")
	assert.NotContains(t, names, "app_tool", "an app-visible-only tool must NEVER reach buildToolDefs' output")
}
