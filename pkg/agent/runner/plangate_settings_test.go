package runner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Turning the gate on IS the intent to make agents plan. Leaving requirePlan a
// separate opt-in defaulting false meant a cluster could run `mode: logging`,
// look gated, and collect nothing — which is exactly what happened: five live
// sessions, every call attributed to the implicit phase, because no agent ever
// planned and nothing required it to.
//
// So UNSET derives from the mode. Explicit false remains an opt-out, because
// gate-on-but-observe-only is a real rollout stage: it records which handles
// calls actually resolve to without changing what the agent may do.
func TestPlanGateRequirePlan_derivesFromMode(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		requirePlan *bool
		want        bool
	}{
		{name: "logging, unset: required (turning the gate on means plan)", mode: "logging", want: true},
		{name: "enforcing, unset: required", mode: "enforcing", want: true},
		{name: "disabled, unset: not required (the gate does not run at all)", mode: "disabled", want: false},

		{name: "logging, explicit false: observe-only opt-out honoured", mode: "logging", requirePlan: ptr.To(false), want: false},
		{name: "enforcing, explicit false: honoured", mode: "enforcing", requirePlan: ptr.To(false), want: false},
		{name: "logging, explicit true: unchanged", mode: "logging", requirePlan: ptr.To(true), want: true},

		// A gate that does not run cannot refuse anything, so an explicit true
		// here would promise a denial that never arrives.
		{name: "disabled, explicit true: still not required", mode: "disabled", requirePlan: ptr.To(true), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			es := &spiceboxv1alpha1.EffectiveSettings{}
			es.Authz.PlanGate = &spiceboxv1alpha1.PlanGateConfig{
				Mode:        tc.mode,
				RequirePlan: tc.requirePlan,
			}
			assert.Equal(t, tc.want, runner.PlanGateRequirePlan(es))
		})
	}
}

// No settings at all, and no plan-gate block, both mean the gate is off.
func TestPlanGateRequirePlan_absentSettingsMeanNotRequired(t *testing.T) {
	assert.False(t, runner.PlanGateRequirePlan(nil))
	assert.False(t, runner.PlanGateRequirePlan(&spiceboxv1alpha1.EffectiveSettings{}))
}
