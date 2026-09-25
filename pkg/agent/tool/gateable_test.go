package tool_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// The startup log reports how many tools the plan gate can gate. It counted
// tools with a BASE handle, which for a sandbox tool is the toolkit default —
// so a gh tool whose subcommands carry real checks reported as ungateable and
// the line read `surfaceHandles=2 gatedTools=0`. That contradiction is worse
// than no number: it says the gate found handles it cannot use.
func TestCanBeGated_countsAToolGateableThroughAVariant(t *testing.T) {
	assert.True(t, tool.CanBeGated(variantOnlyTool{}),
		"a passthrough base with a checked variant IS gateable — per call")
	assert.False(t, tool.CanBeGated(plainTool{}),
		"passthrough with no variants is not a plan-gate concern")
}

type fakeBase struct{}

func (fakeBase) Name() string                 { return "t" }
func (fakeBase) Kind() tool.Kind              { return tool.KindSandbox }
func (fakeBase) Description() string          { return "t" }
func (fakeBase) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (fakeBase) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}
func (fakeBase) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (fakeBase) PermissionVariants() []authz.PermissionVariant { return nil }

type plainTool struct{ fakeBase }

type variantOnlyTool struct{ fakeBase }

func (variantOnlyTool) PermissionVariants() []authz.PermissionVariant {
	return []authz.PermissionVariant{{
		When: `args.args[0] == "pr"`,
		Check: authz.Permission{
			StateImpact: authz.External,
			Check:       &authz.PermissionCheck{ResourceType: "github_repo", Permission: "write"},
		},
	}}
}
