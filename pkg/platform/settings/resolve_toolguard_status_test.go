// pkg/platform/settings/resolve_toolguard_status_test.go
//
// The Defaults-side half of the "a half-authored rate pair must never reach
// status" invariant. The ceiling side lives in resolve_toolguard_test.go
// (TestFoldToolGuardCeiling_KeepsEveryTiersBound); this file covers the tier
// policies effectiveToolGuard mirrors into status.effectiveSettings verbatim.
package settings

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ratePairs reports the (maxCalls present, window present) shape of every rule
// in a mirrored policy — the exact predicate the CEL rule on RateLimitSpec
// evaluates as has(self.maxCalls) == has(self.window).
func ratePairs(t *testing.T, p *v1.ToolGuardPolicy) [][2]bool {
	t.Helper()
	require.NotNil(t, p)
	var out [][2]bool
	for _, r := range p.Rules {
		if r.RateLimit == nil {
			continue
		}
		out = append(out, [2]bool{r.RateLimit.MaxCalls != 0, r.RateLimit.Window != nil})
	}
	return out
}

// tierWith builds a SettingsSpec whose Defaults carry one toolGuard rule with
// the given rate limit — the shape a cluster admin authors by hand.
func tierWith(rl *v1.RateLimitSpec) *v1.SettingsSpec {
	return &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{
		ToolGuard: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
			{Match: v1.ToolGuardMatch{Tool: "*"}, RateLimit: rl},
		}},
	}}
}

// TestEffectiveToolGuardDropsHalfAuthoredRateLimit pins that a rate limit
// carrying only one half of the maxCalls/window pair never reaches
// status.effectiveSettings.
//
// The pair enforces or nothing (v1.CallRate → +Inf when either half is missing,
// and ResolvedRule.HasRateLimit requires both positive), so an orphan half is a
// rule that is legal to author and silently unenforced. Mirroring it into status
// untouched would violate RateLimitSpec's pairing CEL rule and make the whole
// status write inadmissible, wedging every reconcile in the namespace over a
// field on a different object. It is dropped and reported on the way through,
// exactly as the ceiling side does (tierRateBounds).
func TestEffectiveToolGuardDropsHalfAuthoredRateLimit(t *testing.T) {
	window := &metav1.Duration{Duration: time.Minute}

	cases := []struct {
		name string
		rl   *v1.RateLimitSpec
		// wantPairs is the per-rule (maxCalls, window) presence the mirrored
		// policy must show; both-or-neither is the admissibility bar.
		wantPairs   [][2]bool
		wantReports int
	}{
		{
			name:        "maxCalls without window: half dropped, one warning",
			rl:          &v1.RateLimitSpec{MaxCalls: 100, Action: "deny"},
			wantPairs:   [][2]bool{{false, false}},
			wantReports: 1,
		},
		{
			name:        "window without maxCalls: half dropped, one warning",
			rl:          &v1.RateLimitSpec{Window: window, Action: "deny"},
			wantPairs:   [][2]bool{{false, false}},
			wantReports: 1,
		},
		{
			name:      "both halves authored: carried through untouched, no warning",
			rl:        &v1.RateLimitSpec{MaxCalls: 100, Window: window},
			wantPairs: [][2]bool{{true, true}},
		},
		{
			name:      "neither half (per-turn cap only): carried through untouched, no warning",
			rl:        &v1.RateLimitSpec{MaxCallsPerTurn: 4},
			wantPairs: [][2]bool{{false, false}},
		},
	}

	for _, tc := range cases {
		for _, tier := range []string{"cluster", "namespace"} {
			t.Run(tier+": "+tc.name, func(t *testing.T) {
				in := Inputs{}
				spec := tierWith(tc.rl)
				if tier == "cluster" {
					in.Cluster = spec
				} else {
					in.Namespace = spec
				}

				eff, vs := Resolve(in)
				require.NotNil(t, eff.ToolGuard)
				got := eff.ToolGuard.Cluster
				if tier == "namespace" {
					got = eff.ToolGuard.Namespace
				}

				assert.Equal(t, tc.wantPairs, ratePairs(t, got),
					"a mirrored rateLimit must carry both halves of the pair or neither")
				assert.Empty(t, fatalOf(vs), "an unenforceable rate limit is a warning, never session-blocking")
				assert.Len(t, vs, tc.wantReports, "every dropped half must be reported, not swallowed")
				for _, v := range vs {
					assert.Equal(t, ReasonToolGuardRateBoundUnenforceable, v.Reason)
					assert.Contains(t, v.Message, tier, "the report must name the tier that authored it")
				}

				// Whatever else changed, the halves that DO enforce something
				// must survive: dropping an orphan must never relax a real cap.
				require.Len(t, got.Rules, 1)
				assert.Equal(t, tc.rl.MaxCallsPerTurn, got.Rules[0].RateLimit.MaxCallsPerTurn)
				assert.Equal(t, tc.rl.Action, got.Rules[0].RateLimit.Action)
			})
		}
	}
}

// TestEffectiveToolGuardDoesNotMutateItsInput pins that sanitizing a tier's
// policy leaves the caller's SettingsSpec alone. Resolve is documented pure,
// and the pointer it is handed aliases the SettingsSpec the reconciler read —
// editing it in place would corrupt the object the caller still holds.
func TestEffectiveToolGuardDoesNotMutateItsInput(t *testing.T) {
	spec := tierWith(&v1.RateLimitSpec{MaxCalls: 100})
	in := Inputs{Cluster: spec}

	_, vs := Resolve(in)
	require.Len(t, vs, 1, "the half-authored pair must be reported")

	assert.Equal(t, int32(100), spec.Defaults.ToolGuard.Rules[0].RateLimit.MaxCalls,
		"the caller's spec must be untouched")
}
