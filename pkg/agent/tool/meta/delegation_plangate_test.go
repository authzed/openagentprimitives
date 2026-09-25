package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// delegate is plan-gate-governed because, in its own words, "a parent under
// injection escapes its own plan by delegating a write it cannot itself
// perform." The two tools that KEEP DIRECTING a live child carried the same
// bare Stateless permission and no marker, so the plan gate never saw them.
//
// The escape: phase 1 of the approved plan carries tool:delegate and the parent
// spawns a conversational child. The parent then advances into a narrow phase
// whose ceiling does NOT carry tool:delegate. Once the child parks on a
// question — the normal chat-mode flow — the parent hands it an arbitrary new
// instruction through reply_to_subagent (8192 runes of parent-authored text),
// or hands it data through send_input. A delegate call carrying that same
// instruction in that same phase raises a plan_amendment card; these raised
// nothing, were written to no plan-gate audit record, and were charged against
// no phase budget.
//
// The fix is the marker only: both stay Stateless at dispatch, so no SpiceDB
// check and no per-call approval is added to either.
func TestDirectingALiveChildIsPlanGateGoverned(t *testing.T) {
	cases := []struct {
		name   string
		tl     tool.Tool
		handle string
	}{
		{"delegate: spawning a child", &delegateTool{}, "tool:delegate"},
		{"reply_to_subagent: re-instructing a parked child", &subagentReplyTool{}, "tool:reply_to_subagent"},
		{"send_input: handing a parked child data", &sendInputTool{}, "tool:send_input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, ok := tc.tl.(tool.PlanGateGoverned)
			require.True(t, ok, "the tool must carry the PlanGateGoverned marker or the gate never sees the call")
			assert.True(t, g.PlanGateGoverned())

			h, ok := tool.PlanGateHandle(tc.tl)
			require.True(t, ok, "the plan gate must be handed a handle so it can govern this")
			assert.Equal(t, tc.handle, h.String())

			// Dispatch is unchanged: the marker must not add a SpiceDB check
			// or a per-call approval to a tool that reaches no resource.
			assert.Equal(t, authz.Stateless, tc.tl.Permission().StateImpact,
				"dispatch stays Stateless: the marker governs the PLAN, not the call")
			_, mints := tool.BaseHandle(tc.tl)
			assert.False(t, mints, "a Stateless tool mints no dispatch handle")
		})
	}
}
