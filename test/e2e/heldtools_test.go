//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// namedTool is the minimum partitionHeldTools reads: a name.
type namedTool struct{ name string }

func (n *namedTool) Name() string                                  { return n.name }
func (n *namedTool) Kind() tool.Kind                               { return tool.KindMCP }
func (n *namedTool) Description() string                           { return "held-tools test tool" }
func (n *namedTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{"type":"object"}`) }
func (n *namedTool) Permission() authz.Permission                  { return authz.Permission{} }
func (n *namedTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (n *namedTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}

func toolsNamed(names ...string) []tool.Tool {
	out := make([]tool.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, &namedTool{name: n})
	}
	return out
}

func namesOf(tools []tool.Tool) []string {
	if tools == nil {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}

func TestPartitionHeldTools(t *testing.T) {
	cases := []struct {
		name     string
		all      []string
		hold     []string
		wantKept []string
		wantHeld []string
	}{
		{
			name:     "no hold list: every scenario but a replay is untouched",
			all:      []string{"a", "b"},
			hold:     nil,
			wantKept: []string{"a", "b"},
			wantHeld: nil,
		},
		{
			name:     "held tools come out, in the order they were in",
			all:      []string{"a", "box_read", "b", "box_write"},
			hold:     []string{"box_write", "box_read"},
			wantKept: []string{"a", "b"},
			wantHeld: []string{"box_read", "box_write"},
		},
		{
			name:     "a hold name matching nothing holds nothing back",
			all:      []string{"a", "b"},
			hold:     []string{"never_composed"},
			wantKept: []string{"a", "b"},
			wantHeld: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept, held := partitionHeldTools(toolsNamed(tc.all...), tc.hold)
			assert.Equal(t, tc.wantKept, namesOf(kept),
				"LLM-facing order is part of what a replay reproduces")
			assert.Equal(t, tc.wantHeld, namesOf(held))
		})
	}
}
