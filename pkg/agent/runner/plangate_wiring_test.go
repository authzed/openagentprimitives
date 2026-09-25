package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The gate activates on its OWN resolved mode. Registering it behind
// ToolCallAuthz would make it evaporate silently whenever tool-call authz is
// permissive or disabled — the exact failure this table guards.
func TestPlanGateRegistration_activatesOnItsOwnMode(t *testing.T) {
	cases := []struct {
		name         string
		planGateMode string
		toolAuthMode string
		wantPresent  bool
	}{
		{"unset mode: not registered", "", "enforcing", false},
		{"disabled: not registered", "disabled", "enforcing", false},
		{"logging: registered", "logging", "enforcing", true},
		{"enforcing: registered", "enforcing", "enforcing", true},

		// The independence rows.
		{"logging while toolCalls is permissive: still registered", "logging", "permissive", true},
		{"logging while toolCalls is disabled: still registered", "logging", "disabled", true},
		{"disabled while toolCalls enforces: still absent", "disabled", "enforcing", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &Loop{PlanGateMode: tc.planGateMode, ToolAuthMode: tc.toolAuthMode}
			names := hookNamesAt(l.buildPipelineRegistry(), pipeline.PreToolCall)
			if tc.wantPresent {
				assert.Contains(t, names, "plan_gate")
			} else {
				assert.NotContains(t, names, "plan_gate")
			}
		})
	}
}

// Order 19 puts the gate after content inspection and before ToolCallAuthz:
// blocked content never reaches the gate, and an out-of-ceiling call is refused
// before it burns a SpiceDB check or raises an approval ask.
func TestPlanGateRegistration_runsAfterContentGuardAndBeforeToolCallAuthz(t *testing.T) {
	assert.Greater(t, hooks.OrderPlanGate, hooks.OrderContentGuard)
	assert.Less(t, hooks.OrderPlanGate, hooks.OrderToolCallAuthz)

	l := &Loop{PlanGateMode: "logging", ToolAuthMode: "enforcing"}
	names := hookNamesAt(l.buildPipelineRegistry(), pipeline.PreToolCall)

	planIdx, authzIdx := indexOf(names, "plan_gate"), indexOf(names, "tool_call_authz")
	require.GreaterOrEqual(t, planIdx, 0, "plan_gate must be registered")
	require.GreaterOrEqual(t, authzIdx, 0, "tool_call_authz must be registered")
	assert.Less(t, planIdx, authzIdx, "plan_gate must run before tool_call_authz")
}

// The gate attaches at PreToolCall only. Appearing at PostToolCall would double
// count every call in the logging dataset.
func TestPlanGateRegistration_onlyAtPreToolCall(t *testing.T) {
	l := &Loop{PlanGateMode: "logging"}
	reg := l.buildPipelineRegistry()

	assert.Contains(t, hookNamesAt(reg, pipeline.PreToolCall), "plan_gate")
	for _, p := range []pipeline.Point{
		pipeline.PostToolCall, pipeline.SessionStart, pipeline.InboundTurn,
		pipeline.PreResponse, pipeline.SessionEnd,
	} {
		assert.NotContains(t, hookNamesAt(reg, p), "plan_gate", "point %s", p)
	}
}

// resolvedPlanGateMode's contract, at the runner boundary: the mode comes from
// the RESOLVED snapshot, so a class that declared a laxer mode than a tier floor
// cannot reach the Loop with it. A nil snapshot resolves to off.
func TestPlanGateRegistration_readsResolvedSettingsNotTheClassSpec(t *testing.T) {
	// A class asking for disabled, while the resolved snapshot says logging —
	// which is what a cluster floor produces. The resolved value must win.
	l := &Loop{
		PlanGateMode: "logging",
		AgentClass: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Authz: &spiceboxv1alpha1.AuthzBlock{
					PlanGate: &spiceboxv1alpha1.PlanGateConfig{Mode: "disabled"},
				},
			},
		},
	}
	assert.Contains(t, hookNamesAt(l.buildPipelineRegistry(), pipeline.PreToolCall), "plan_gate",
		"the clamped resolved mode must win over the class's own laxer request")
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
