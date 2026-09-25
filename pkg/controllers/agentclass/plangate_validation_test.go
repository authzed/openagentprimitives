package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func classWith(planGate, toolCalls string) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{}
	if planGate != "" {
		ac.Spec.Authz.PlanGate = &spiceboxv1alpha1.PlanGateConfig{Mode: planGate}
	}
	if toolCalls != "" {
		ac.Spec.Authz.ToolCalls = &spiceboxv1alpha1.ToolCallsAuthz{Mode: toolCalls}
	}
	return ac
}

// planGate: enforcing enforces the CLASS axis itself, but the INSTANCE axis —
// which resource a call may touch — rides entirely on tool_call_authz. With
// that hook permissive or disabled, per-resource gating silently stops while
// the class axis keeps refusing, which looks from outside like the gate working.
//
// A validation error rather than a silent narrowing: the operator asked for
// enforcement and would otherwise get half of it without being told.
func TestValidatePlanGateMode(t *testing.T) {
	cases := []struct {
		name               string
		planGate, toolCall string
		wantReason         bool
	}{
		{"enforcing + enforcing: valid", "enforcing", "enforcing", false},
		{"enforcing + permissive: rejected", "enforcing", "permissive", true},
		{"enforcing + disabled: rejected", "enforcing", "disabled", true},
		{"enforcing + unset: rejected unless the default is enforcing", "enforcing", "", false},

		// The constraint is specific to enforcing. Logging enforces nothing, so
		// it cannot silently half-enforce.
		{"logging + permissive: valid", "logging", "permissive", false},
		{"logging + disabled: valid", "logging", "disabled", false},
		{"disabled + disabled: valid", "disabled", "disabled", false},
		{"unset + permissive: valid", "", "permissive", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validatePlanGateMode(classWith(tc.planGate, tc.toolCall))
			if tc.wantReason {
				assert.Equal(t, spiceboxv1alpha1.ReasonPlanGateRequiresToolCallEnforcing, reason)
				assert.Contains(t, msg, "toolCalls.mode",
					"the message must name the field to change")
				assert.Contains(t, msg, "logging",
					"and offer the other way out, not just the strict one")
				return
			}
			assert.Empty(t, reason, "unexpected rejection: %s", msg)
		})
	}
}

// The message has to be actionable: an operator reading it must know both
// remedies without opening the spec.
func TestValidatePlanGateMode_messageOffersBothRemedies(t *testing.T) {
	_, msg := validatePlanGateMode(classWith("enforcing", "permissive"))

	assert.Contains(t, msg, "Set authz.toolCalls.mode: enforcing")
	assert.Contains(t, msg, "lower planGate.mode to logging")
}
