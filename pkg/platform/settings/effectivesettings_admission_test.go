//go:build integration

// pkg/platform/settings/effectivesettings_admission_test.go
//
// The end of the chain the unit tests approximate: what Resolve stamps onto
// status.effectiveSettings must be ACCEPTED by the apiserver.
//
// status.effectiveSettings mirrors tier ToolGuard policies, so every CEL rule on
// a type reachable from spec.toolGuard is ALSO attached to a status path, and it
// governs the status write of objects stored before it existed and never
// re-validated — a rejected status write wedges the reconcile with an error
// naming a field on a different object. Only a real apiserver can prove the
// installed CRD accepts what the resolver produces, so this runs against envtest
// rather than re-deriving the rule in Go.
package settings_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
)

// legacyTier builds a tier spec carrying a sliding-window cap with only one half
// of the maxCalls/window pair — the shape a stored object can still hold, since
// RateLimitSpec's pairing CEL rule never re-validates it.
//
// The budget default is unrelated to what is under test and only keeps the
// assertion sharp: EffectiveSettings.Budget is a required status field with
// Minimum=1 per dimension, so a tier chain configuring no budget would be
// rejected for that reason too, burying the rate-limit failure under test.
func legacyTier(rl *v1.RateLimitSpec) *v1.SettingsSpec {
	return &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{
		Budget: &v1.BudgetConfig{MaxTurns: 10, MaxTokens: 100000},
		ToolGuard: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
			{Match: v1.ToolGuardMatch{Tool: "*"}, RateLimit: rl},
		}},
	}}
}

func TestEffectiveSettingsStatusWriteIsAdmissible(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	window := &metav1.Duration{Duration: time.Minute}

	cases := []struct {
		name string
		rl   *v1.RateLimitSpec
	}{
		{name: "pre-existing maxCalls without window: status still accepted", rl: &v1.RateLimitSpec{MaxCalls: 100, Action: "deny"}},
		{name: "pre-existing window without maxCalls: status still accepted", rl: &v1.RateLimitSpec{Window: window, Action: "deny"}},
		{name: "well-formed pair: status accepted and the cap survives", rl: &v1.RateLimitSpec{MaxCalls: 100, Window: window}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "admissible-" + string(rune('a'+i))
			sess := &v1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: v1.AgentSessionSpec{
					Class:  "demo-agent",
					Prompt: v1.PromptSource{Inline: "hello"},
				},
			}
			require.NoError(t, env.Client.Create(ctx, sess), "creating the session under test")

			eff, vs := settings.Resolve(settings.Inputs{Cluster: legacyTier(tc.rl)})
			for _, v := range vs {
				assert.False(t, v.Fatal, "an unenforceable rate limit must never be session-blocking: %s", v.Message)
			}

			status := eff.ToStatus()
			sess.Status.EffectiveSettings = &status
			require.NoError(t, env.Client.Status().Update(ctx, sess),
				"the resolver's own output must be admissible against the installed CRD")

			var got v1.AgentSession
			require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &got))
			require.NotNil(t, got.Status.EffectiveSettings)
			require.NotNil(t, got.Status.EffectiveSettings.ToolGuard)
			require.NotNil(t, got.Status.EffectiveSettings.ToolGuard.Cluster)
			require.Len(t, got.Status.EffectiveSettings.ToolGuard.Cluster.Rules, 1)

			rl := got.Status.EffectiveSettings.ToolGuard.Cluster.Rules[0].RateLimit
			require.NotNil(t, rl)
			assert.Equal(t, rl.MaxCalls != 0, rl.Window != nil,
				"the stored pair must be both-or-neither, matching the CEL rule on RateLimitSpec")
			if tc.rl.MaxCalls != 0 && tc.rl.Window != nil {
				assert.Equal(t, tc.rl.MaxCalls, rl.MaxCalls, "an enforceable cap must survive the round trip")
			}
		})
	}
}
