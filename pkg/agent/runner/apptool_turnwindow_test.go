package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
)

// TestAppToolExecCtx_EachCallOpensItsOwnTurnWindow asserts that two successive
// widget (proxy-exec) app-tool calls are not billed to the same tool-guard
// per-turn window. A widget call is out-of-band from the LLM turn stream, so
// there is no turn to roll over on its own; leaving the synthetic IDs at
// TurnIndex 0 made Registry.Admit treat every widget call the session ever
// makes as one endless turn, permanently denying them once a maxCallsPerTurn
// ceiling is configured — with a message claiming the budget was exhausted
// "this turn".
func TestAppToolExecCtx_EachCallOpensItsOwnTurnWindow(t *testing.T) {
	l := &Loop{}
	sess := &tool.SessionContext{Namespace: "default", Name: "aw1"}
	deps := l.toolGuardDeps()
	reg := toolguard.NewRegistry(nil)
	rule := toolguard.ResolvedRule{RateMaxPerTurn: 1, RateAction: toolguard.ActionDeny}

	admitAppToolCall := func(useID string) toolguard.Admission {
		t.Helper()
		ctx := l.appToolExecCtx(context.Background(), sess, useID)
		adm, _ := reg.Admit(ctx, toolguard.ToolKey("do_thing"), "", rule, deps.TurnIndex(ctx))
		return adm
	}

	first := admitAppToolCall("op-1")
	require.True(t, first.Allowed, "the first widget call must be admitted")

	second := admitAppToolCall("op-2")
	assert.True(t, second.Allowed,
		"a later widget call is a new window, not a second call inside the first one; denied by %q", second.DeniedBy)
}
