package runner

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func TestToolGuardHooksRegisteredWhenPolicySet(t *testing.T) {
	pol, err := toolguard.ResolvePolicy(toolguard.Tiers{})
	require.NoError(t, err)
	l := &Loop{ToolGuardPolicy: pol}
	reg := l.buildPipelineRegistry()

	names := func(p pipeline.Point) []string {
		var out []string
		for _, h := range reg.Hooks(p) {
			out = append(out, h.Name())
		}
		return out
	}
	assert.Contains(t, names(pipeline.PreToolCall), "tool_guard")
	assert.Contains(t, names(pipeline.PostToolCall), "tool_guard_record")

	// OrderToolGuard=15 must keep the guard ahead of tool_call_authz (20) so
	// a breaker-open denial never burns a SpiceDB check or approval ask.
	pre := names(pipeline.PreToolCall)
	assert.Less(t, slices.Index(pre, "tool_guard"), slices.Index(pre, "tool_call_authz"))
}

func TestToolGuardHooksAbsentWhenPolicyNil(t *testing.T) {
	l := &Loop{}
	reg := l.buildPipelineRegistry()
	for _, h := range reg.Hooks(pipeline.PreToolCall) {
		assert.NotEqual(t, "tool_guard", h.Name())
	}
}
