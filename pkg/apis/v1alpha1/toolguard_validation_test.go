//go:build integration

// pkg/apis/v1alpha1/toolguard_validation_test.go
//
// Integration test for the CEL validations pairing maxCalls with window, on
// both surfaces that carry the pair:
//   - Limits.ToolGuard (ToolGuardCeiling) — AgentSettings
//   - spec.toolGuard rules' rateLimit (RateLimitSpec) — AgentClass
//
// Both consumers enforce the pair or nothing (pkg/authz/toolguard's clamp and
// pkg/platform/settings' fold both skip a ceiling missing either half, and
// ResolvedRule.HasRateLimit requires both to be positive), so a half-authored
// pair used to be admitted, reported by no reconciler, and enforce nothing —
// with a doc comment as the only thing carrying the invariant.
//
// Runs against a real envtest apiserver so it exercises the INSTALLED CRD, not
// a Go-level pre-check. NOTE: config/crds must be regenerated (mage gen:api +
// mage manifests) for this to pass — that regeneration is owed by the commit
// that added the markers.
package v1alpha1_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// settingsWithCeiling builds a namespace-tier AgentSettings whose Limits carry
// only the rate half of a ToolGuardCeiling.
func settingsWithCeiling(namespace string, ceiling *v1alpha1.ToolGuardCeiling) *v1alpha1.AgentSettings {
	return &v1alpha1.AgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.AgentSettingsName, Namespace: namespace},
		Spec: v1alpha1.SettingsSpec{
			Limits: &v1alpha1.SettingsLimits{ToolGuard: ceiling},
		},
	}
}

// classWithRateLimit builds a minimally-valid AgentClass (a systemPrompt is the
// only spec-required field) carrying one toolGuard rule with the given rate
// limit.
func classWithRateLimit(name string, rl *v1alpha1.RateLimitSpec) *v1alpha1.AgentClass {
	return &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.AgentClassSpec{
			SystemPrompt: v1alpha1.PromptSource{Inline: "you are an agent"},
			ToolGuard: &v1alpha1.ToolGuardPolicy{Rules: []v1alpha1.ToolGuardRule{
				{Match: v1alpha1.ToolGuardMatch{Tool: "*"}, RateLimit: rl},
			}},
		},
	}
}

func i32p(v int32) *int32 { return &v }

func TestToolGuardRatePairingCEL(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	window := &metav1.Duration{Duration: time.Minute}

	t.Run("ceiling maxCalls without window: rejected", func(t *testing.T) {
		err := env.Client.Create(ctx, settingsWithCeiling("default",
			&v1alpha1.ToolGuardCeiling{MaxCalls: i32p(5)}))
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
		assert.Contains(t, err.Error(), "toolGuard.maxCalls and toolGuard.window must be set together")
	})

	t.Run("ceiling window without maxCalls: rejected", func(t *testing.T) {
		err := env.Client.Create(ctx, settingsWithCeiling("default",
			&v1alpha1.ToolGuardCeiling{Window: window}))
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
		assert.Contains(t, err.Error(), "toolGuard.maxCalls and toolGuard.window must be set together")
	})

	t.Run("ceiling with both: accepted", func(t *testing.T) {
		assert.NoError(t, env.Client.Create(ctx, settingsWithCeiling("default",
			&v1alpha1.ToolGuardCeiling{MaxCalls: i32p(5), Window: window})))
	})

	t.Run("rule rateLimit maxCalls without window: rejected", func(t *testing.T) {
		err := env.Client.Create(ctx, classWithRateLimit("rate-no-window",
			&v1alpha1.RateLimitSpec{MaxCalls: 5}))
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
		assert.Contains(t, err.Error(), "rateLimit.maxCalls and rateLimit.window must be set together")
	})

	t.Run("rule rateLimit with neither (per-turn cap only): accepted", func(t *testing.T) {
		assert.NoError(t, env.Client.Create(ctx, classWithRateLimit("rate-per-turn-only",
			&v1alpha1.RateLimitSpec{MaxCallsPerTurn: 4})))
	})

	t.Run("rule rateLimit with both: accepted", func(t *testing.T) {
		assert.NoError(t, env.Client.Create(ctx, classWithRateLimit("rate-well-formed",
			&v1alpha1.RateLimitSpec{MaxCalls: 5, Window: window})))
	})
}
