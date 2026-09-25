package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func strPtr(s string) *string { return &s }

func TestPinRequirementForFoldsTiersStrongestWins(t *testing.T) {
	cases := []struct {
		name        string
		cluster     *v1.PinningPolicy
		namespace   *v1.PinningPolicy
		kind, item  string
		minStrength string
		mode        string
		bypassWhy   string
	}{
		{
			name: "no policy anywhere: no requirement",
			kind: "mcp", item: "gh",
			minStrength: "", mode: "",
		},
		{
			name:    "single cluster rule, empty mode defaults to approve",
			cluster: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen"}}},
			kind:    "mcp", item: "gh",
			minStrength: "frozen", mode: "approve",
		},
		{
			name:      "strongest strength and mode win across tiers",
			cluster:   &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "image", MinStrength: "named", Mode: "warn"}}},
			namespace: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "image", MinStrength: "frozen", Mode: "block"}}},
			kind:      "image", item: "ghcr.io/org/toolbox",
			minStrength: "frozen", mode: "block",
		},
		{
			name:      "namespace bypass shields the namespace rule only — cluster rule survives",
			cluster:   &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen", Mode: "warn"}}},
			namespace: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen", Mode: "block"}}, Bypass: []v1.PinningBypass{{Kind: "mcp", Name: "gh", Reason: "migration week"}}},
			kind:      "mcp", item: "gh",
			minStrength: "frozen", mode: "warn", bypassWhy: "migration week",
		},
		{
			name:      "cluster bypass shields both tiers",
			cluster:   &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen", Mode: "block"}}, Bypass: []v1.PinningBypass{{Kind: "mcp", Name: "gh", Reason: "emergency"}}},
			namespace: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen", Mode: "block"}}},
			kind:      "mcp", item: "gh",
			minStrength: "", mode: "", bypassWhy: "emergency",
		},
		{
			name:    "bypass matches by trailing-wildcard pattern",
			cluster: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "skill", MinStrength: "frozen", Mode: "block"}}, Bypass: []v1.PinningBypass{{Kind: "skill", Name: "github.com/org/**", Reason: "trusted org"}}},
			kind:    "skill", item: "github.com/org/repo//skills/x@v1",
			minStrength: "", mode: "", bypassWhy: "trusted org",
		},
		{
			name:    "bypass for a different item does not shield",
			cluster: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen", Mode: "block"}}, Bypass: []v1.PinningBypass{{Kind: "mcp", Name: "other", Reason: "x"}}},
			kind:    "mcp", item: "gh",
			minStrength: "frozen", mode: "block",
		},
		{
			name:    "rule for a different kind does not apply",
			cluster: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "image", MinStrength: "frozen", Mode: "block"}}},
			kind:    "mcp", item: "gh",
			minStrength: "", mode: "",
		},
		{
			name:    "explicit off mode is preserved (observe-only)",
			cluster: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "cli", Mode: "off"}}},
			kind:    "cli", item: "gh-cli",
			minStrength: "", mode: "off",
		},
		{
			name:      "cluster bypass shields a namespace-only rule",
			cluster:   &v1.PinningPolicy{Bypass: []v1.PinningBypass{{Kind: "mcp", Name: "gh", Reason: "emergency"}}},
			namespace: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen", Mode: "block"}}},
			kind:      "mcp", item: "gh",
			minStrength: "", mode: "", bypassWhy: "emergency",
		},
		{
			name:    "bypass kind mismatch does not shield",
			cluster: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen", Mode: "block"}}, Bypass: []v1.PinningBypass{{Kind: "skill", Name: "gh", Reason: "x"}}},
			kind:    "mcp", item: "gh",
			minStrength: "frozen", mode: "block",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := PinRequirementFor(tc.cluster, tc.namespace, tc.kind, tc.item)
			assert.Equal(t, tc.minStrength, req.MinStrength, "minStrength")
			assert.Equal(t, tc.mode, req.Mode, "mode")
			assert.Equal(t, tc.bypassWhy, req.BypassReason, "bypassReason")
		})
	}
}

func TestTierPinningPolicy(t *testing.T) {
	t.Run("a tier with rules yields a copy the caller may mutate", func(t *testing.T) {
		spec := &v1.SettingsSpec{Limits: &v1.SettingsLimits{
			Pinning: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "skill", MinStrength: "frozen", Mode: v1.PinModeBlock}}},
		}}
		pol := tierPinningPolicy(spec)
		require.NotNil(t, pol)
		assert.Equal(t, []v1.PinningRule{{Kind: "skill", MinStrength: "frozen", Mode: v1.PinModeBlock}}, pol.Rules)

		pol.Rules[0].MinStrength = "named"
		assert.Equal(t, "frozen", spec.Limits.Pinning.Rules[0].MinStrength, "the source spec must not be aliased")
	})
	t.Run("a tier with no pinning yields nil", func(t *testing.T) {
		assert.Nil(t, tierPinningPolicy(nil))
		assert.Nil(t, tierPinningPolicy(&v1.SettingsSpec{}))
		assert.Nil(t, tierPinningPolicy(&v1.SettingsSpec{Limits: &v1.SettingsLimits{}}))
	})
}

func violationReasons(vs []Violation) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Reason)
	}
	return out
}

func findViolation(t *testing.T, vs []Violation, reason string) Violation {
	t.Helper()
	for _, v := range vs {
		if v.Reason == reason {
			return v
		}
	}
	t.Fatalf("no violation with reason %q in %v", reason, violationReasons(vs))
	return Violation{}
}

func TestCheckPinningSkillFloorIsFatalAndSpeaksSkillVocabulary(t *testing.T) {
	// A block/frozen skill floor + a tag-pinned skill → fatal
	// SkillPinningRequired, worded in the @sha/@tag vocabulary a skill author
	// actually types rather than the internal strength name.
	in := Inputs{
		Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{
			Pinning: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "skill", MinStrength: "frozen", Mode: v1.PinModeBlock}}},
		}},
		ClassSkills: []string{"github.com/org/repo//skills/x@v1.2.0"},
	}
	vs := checkPinning(in, effectivePinning(in))
	v := findViolation(t, vs, ReasonSkillPinningRequired)
	assert.True(t, v.Fatal)
	assert.Equal(t, "skill github.com/org/repo//skills/x@v1.2.0 must be pinned to at least sha", v.Message)
}

func TestCheckPinningSkillRollingWarningAlwaysEmitted(t *testing.T) {
	// An unpinned skill warns even with no pinning policy configured at all.
	in := Inputs{ClassSkills: []string{"github.com/org/repo//skills/x"}}
	vs := checkPinning(in, effectivePinning(in))
	v := findViolation(t, vs, ReasonSkillRolling)
	assert.False(t, v.Fatal)
	assert.Equal(t, "skill github.com/org/repo//skills/x tracks a mutable ref; pin to a SHA for reproducibility/security", v.Message)
}

func TestCheckPinningModeGovernsFatality(t *testing.T) {
	classPins := []DeclaredPin{{Kind: "image", Name: "ghcr.io/org/toolbox", Strength: "named"}}
	cases := []struct {
		name      string
		mode      string
		wantCount int
		wantFatal bool
	}{
		{name: "block floor violation is fatal", mode: "block", wantCount: 1, wantFatal: true},
		{name: "approve floor violation is a non-fatal warning", mode: "approve", wantCount: 1, wantFatal: false},
		{name: "default (empty) mode behaves as approve", mode: "", wantCount: 1, wantFatal: false},
		{name: "warn floor violation is a non-fatal warning", mode: "warn", wantCount: 1, wantFatal: false},
		{name: "off emits nothing (observe only)", mode: "off", wantCount: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Inputs{
				Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{Pinning: &v1.PinningPolicy{
					Rules: []v1.PinningRule{{Kind: "image", MinStrength: "frozen", Mode: tc.mode}},
				}}},
				ClassPins: classPins,
			}
			vs := checkPinning(in, effectivePinning(in))
			require.Len(t, vs, tc.wantCount)
			if tc.wantCount > 0 {
				assert.Equal(t, ReasonPinningRequired, vs[0].Reason)
				assert.Equal(t, tc.wantFatal, vs[0].Fatal)
				assert.Equal(t, "image ghcr.io/org/toolbox must be pinned to at least frozen", vs[0].Message)
			}
		})
	}
}

func TestCheckPinningBypassSuppressesFloor(t *testing.T) {
	in := Inputs{
		Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{Pinning: &v1.PinningPolicy{
			Rules:  []v1.PinningRule{{Kind: "mcp", MinStrength: "frozen", Mode: "block"}},
			Bypass: []v1.PinningBypass{{Kind: "mcp", Name: "gh", Reason: "emergency"}},
		}}},
		ClassPins: []DeclaredPin{{Kind: "mcp", Name: "gh", Strength: "unpinned"}},
	}
	assert.Empty(t, checkPinning(in, effectivePinning(in)))
}

func TestCheckPinningSatisfiedFloorIsSilent(t *testing.T) {
	in := Inputs{
		Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{Pinning: &v1.PinningPolicy{
			Rules: []v1.PinningRule{{Kind: "image", MinStrength: "named", Mode: "block"}},
		}}},
		ClassPins: []DeclaredPin{{Kind: "image", Name: "ghcr.io/org/toolbox", Strength: "frozen"}},
	}
	assert.Empty(t, checkPinning(in, effectivePinning(in)))
}

func TestResolveStampsEffectivePinning(t *testing.T) {
	in := Inputs{
		Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{
			Pinning: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "skill", MinStrength: "frozen", Mode: v1.PinModeBlock}}},
		}},
	}
	eff, _ := Resolve(in)
	require.NotNil(t, eff.Pinning)
	require.NotNil(t, eff.Pinning.Cluster)
	assert.Equal(t, []v1.PinningRule{{Kind: "skill", MinStrength: "frozen", Mode: v1.PinModeBlock}}, eff.Pinning.Cluster.Rules)
	assert.Nil(t, eff.Pinning.Namespace)
}

func TestResolveNoPinningPolicyLeavesEffectiveNil(t *testing.T) {
	eff, _ := Resolve(Inputs{})
	assert.Nil(t, eff.Pinning)
}

func TestToStatusCarriesPinning(t *testing.T) {
	eff := EffectiveSettings{Pinning: &v1.EffectivePinning{
		Cluster: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "skill", MinStrength: "frozen", Mode: v1.PinModeBlock}}},
	}}
	st := eff.ToStatus()
	require.NotNil(t, st.Pinning)
	assert.Equal(t, eff.Pinning.Cluster.Rules, st.Pinning.Cluster.Rules)
}

func TestCheckPinningKindsAreIsolated(t *testing.T) {
	// A skill rule must not constrain image pins, and vice versa; mixed
	// declarations flow through declaredPins concatenation untouched.
	in := Inputs{
		Cluster: &v1.SettingsSpec{Limits: &v1.SettingsLimits{
			Pinning: &v1.PinningPolicy{Rules: []v1.PinningRule{
				{Kind: "skill", MinStrength: "frozen", Mode: "block"},
				{Kind: "image", MinStrength: "frozen", Mode: "block"},
			}},
		}},
		ClassSkills: []string{"github.com/org/repo//skills/x@v1.2.0"},
		ClassPins:   []DeclaredPin{{Kind: "image", Name: "ghcr.io/org/toolbox", Strength: "named"}},
	}
	vs := checkPinning(in, effectivePinning(in))
	skillV := findViolation(t, vs, ReasonSkillPinningRequired)
	assert.Equal(t, "skill github.com/org/repo//skills/x@v1.2.0 must be pinned to at least sha", skillV.Message)
	imageV := findViolation(t, vs, ReasonPinningRequired)
	assert.Equal(t, "image ghcr.io/org/toolbox must be pinned to at least frozen", imageV.Message)
	assert.Len(t, vs, 2, "exactly one violation per kind; no cross-kind bleed")
}

func TestDeclaredPinsSkipsMalformedCanonicalNames(t *testing.T) {
	in := Inputs{
		ClassSkills: []string{"not-a-canonical-name", "github.com/org/repo//skills/x@deadbee"},
		ClassPins:   []DeclaredPin{{Kind: "mcp", Name: "gh", Strength: "unpinned"}},
	}
	pins := declaredPins(in)
	require.Len(t, pins, 2, "malformed canonical name is skipped, not errored")
	assert.Equal(t, "skill", pins[0].Kind)
	assert.Equal(t, "mcp", pins[1].Kind)
}

func TestEffectivePinningForSnapshotsRawSpecs(t *testing.T) {
	cluster := &v1.SettingsSpec{Limits: &v1.SettingsLimits{
		Pinning: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "skill", MinStrength: "frozen", Mode: v1.PinModeBlock}}},
	}}
	ns := &v1.SettingsSpec{Limits: &v1.SettingsLimits{
		Pinning: &v1.PinningPolicy{Rules: []v1.PinningRule{{Kind: "mcp", Mode: v1.PinModeWarn}}},
	}}
	eff := EffectivePinningFor(cluster, ns)
	require.NotNil(t, eff)
	assert.Equal(t, []v1.PinningRule{{Kind: "skill", MinStrength: "frozen", Mode: v1.PinModeBlock}}, eff.Cluster.Rules)
	assert.Equal(t, []v1.PinningRule{{Kind: "mcp", Mode: v1.PinModeWarn}}, eff.Namespace.Rules)

	assert.Nil(t, EffectivePinningFor(nil, nil), "no tiers, no snapshot")
}
