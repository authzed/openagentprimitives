package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// The plan gate resolves which handle a call is checked against. It did that
// per TOOL — `tool.BaseHandle`, the tool's base permission — which the code
// comment flagged as deferred work: "arg-aware resolution lands with [narrower
// ceilings]". Sandbox tools make it bite now. One tool covers a whole CLI whose
// base permission is the toolkit default (passthrough), so `gh pr view` had NO
// handle and the gate skipped it as unpermissioned — even though the call
// itself resolves to perm:read:github_repo.
func TestPlanGateResolve_isPerCallNotPerTool(t *testing.T) {
	tl := &perCallTool{}
	l := &Loop{AgentName: "demo-agent", Tools: []tool.Tool{tl}}

	resolve := l.planGateHandleResolver()

	read, ok := resolve("gitlike_gh", map[string]any{"args": []any{"pr", "view"}})
	require.True(t, ok, "a call that resolves to a checked permission has a handle")
	assert.Equal(t, "perm:read:github_repo", read.String())

	write, ok := resolve("gitlike_gh", map[string]any{"args": []any{"pr", "create"}})
	require.True(t, ok)
	assert.Equal(t, "perm:write:github_repo", write.String(),
		"the SAME tool must resolve to a different handle for a different call")
}

// A tool with no per-call answer keeps the base-handle behaviour, so MCP tools
// and meta tools are unaffected.
func TestPlanGateResolve_fallsBackToTheBaseHandle(t *testing.T) {
	l := &Loop{AgentName: "demo-agent", Tools: []tool.Tool{
		echoTool{name: "plain"},
	}}
	_, ok := l.planGateHandleResolver()("plain", nil)
	assert.False(t, ok, "a passthrough tool has no handle and is not a plan-gate concern")
}

var _ = authz.Readonly
