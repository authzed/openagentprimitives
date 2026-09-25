package toolguard

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func mdur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }
func i32(v int32) *int32                    { return &v }
func i64(v int64) *int64                    { return &v }
func sp(s string) *string                   { return &s }

func TestRuleForTierPrecedenceAndMatching(t *testing.T) {
	classPol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{ // disables the breaker for a known-flaky tool
			Match:   v1.ToolGuardMatch{Tool: "flaky_*"},
			Breaker: &v1.BreakerSpec{Action: "off"},
		},
	}}
	nsPol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{
			Match:   v1.ToolGuardMatch{Kind: "mcp"},
			Breaker: &v1.BreakerSpec{FailureThreshold: 2, Action: "halt"},
		},
	}}
	clusterPol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{
			Match:   v1.ToolGuardMatch{Origin: "sidecartoolbox/*"},
			Breaker: &v1.BreakerSpec{FailureThreshold: 7},
		},
	}}
	p, err := ResolvePolicy(Tiers{Class: classPol, Namespace: nsPol, Cluster: clusterPol})
	require.NoError(t, err)

	cases := []struct {
		name             string
		kind, tool, orig string
		check            func(t *testing.T, r ResolvedRule)
	}{
		{
			name: "class rule wins over namespace for matching tool: breaker off",
			kind: "mcp", tool: "flaky_search", orig: "mcpserver/github",
			check: func(t *testing.T, r ResolvedRule) {
				assert.Equal(t, ActionOff, r.Action)
				assert.Equal(t, "class[0]", r.Provenance)
			},
		},
		{
			name: "namespace rule for non-class-matched mcp tool: threshold 2, halt, builtin fills gaps",
			kind: "mcp", tool: "github_search", orig: "mcpserver/github",
			check: func(t *testing.T, r ResolvedRule) {
				assert.Equal(t, int32(2), r.FailureThreshold)
				assert.Equal(t, ActionHalt, r.Action)
				// Unset fields inherit from Builtin.
				assert.Equal(t, Builtin.OriginFailureThreshold, r.OriginFailureThreshold)
				assert.Equal(t, Builtin.InitialCoolOff, r.InitialCoolOff)
				assert.Equal(t, "namespace[0]", r.Provenance)
			},
		},
		{
			name: "cluster origin-glob rule for sidecar tool",
			kind: "mcp", tool: "box_run", orig: "sidecartoolbox/mybox",
			check: func(t *testing.T, r ResolvedRule) {
				// namespace kind=mcp rule matches first (higher tier than cluster).
				assert.Equal(t, "namespace[0]", r.Provenance)
			},
		},
		{
			// path.Match("sidecartoolbox/*", "") is false: prefixed origin
			// globs never match origin-less tools, so they fall through to
			// builtin. Pins the stdlib behavior origin scoping relies on.
			// kind=sandbox is used (not mcp) so the namespace kind=mcp rule
			// doesn't fire first; box_run doesn't match the class flaky_* glob.
			name: "origin-constrained rule skips origin-less tool: builtin",
			kind: "sandbox", tool: "box_run", orig: "",
			check: func(t *testing.T, r ResolvedRule) {
				assert.Equal(t, "builtin", r.Provenance)
			},
		},
		{
			name: "no tier matches: builtin",
			kind: "sandbox", tool: "bash", orig: "",
			check: func(t *testing.T, r ResolvedRule) {
				assert.Equal(t, Builtin.FailureThreshold, r.FailureThreshold)
				assert.Equal(t, ActionDeny, r.Action)
				assert.Equal(t, "builtin", r.Provenance)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, p.RuleFor(tc.kind, tc.tool, tc.orig))
		})
	}
}

func TestRuleForMatchedRuleWithNilBreakerHasNoBreaker(t *testing.T) {
	pol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{
			Match:     v1.ToolGuardMatch{Tool: "search_*"},
			RateLimit: &v1.RateLimitSpec{MaxCallsPerTurn: 4},
		},
	}}
	p, err := ResolvePolicy(Tiers{Class: pol})
	require.NoError(t, err)
	r := p.RuleFor("mcp", "search_web", "mcpserver/g")
	assert.Equal(t, ActionOff, r.Action, "nil Breaker on a matched rule = no breaker")
	assert.Equal(t, int32(4), r.RateMaxPerTurn)
	assert.Equal(t, ActionDeny, r.RateAction, "rate action defaults to deny")
}

func TestCeilingClamps(t *testing.T) {
	pol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{
			Match: v1.ToolGuardMatch{Tool: "x"},
			Breaker: &v1.BreakerSpec{
				FailureThreshold: 50,
				InitialCoolOff:   mdur(time.Second),
				Action:           "off",
			},
		},
	}}
	ceil := &v1.ToolGuardCeiling{
		MaxFailureThreshold: i32(10),
		MinInitialCoolOff:   mdur(time.Minute),
		MinAction:           sp("deny"),
		MaxCallsPerTurn:     i32(30),
	}
	p, err := ResolvePolicy(Tiers{Class: pol, Ceiling: ceil})
	require.NoError(t, err)
	r := p.RuleFor("sandbox", "x", "")
	assert.Equal(t, int32(10), r.FailureThreshold, "threshold clamped down")
	assert.Equal(t, time.Minute, r.InitialCoolOff, "cool-off raised to floor")
	assert.Equal(t, ActionDeny, r.Action, "MinAction floor overrides off")
	assert.Equal(t, int32(30), r.RateMaxPerTurn, "ceiling imposes a rate limit even when unconfigured")
}

func TestCeilingMinActionDoesNotLowerHalt(t *testing.T) {
	pol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{Match: v1.ToolGuardMatch{Tool: "x"}, Breaker: &v1.BreakerSpec{Action: "halt"}},
	}}
	p, err := ResolvePolicy(Tiers{Class: pol, Ceiling: &v1.ToolGuardCeiling{MinAction: sp("deny")}})
	require.NoError(t, err)
	assert.Equal(t, ActionHalt, p.RuleFor("sandbox", "x", "").Action)
}

func TestResolvePolicyRejectsBadGlob(t *testing.T) {
	pol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{Match: v1.ToolGuardMatch{Tool: "[unclosed"}},
	}}
	_, err := ResolvePolicy(Tiers{Class: pol})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "class rule 0")
}

func TestExplicitOffBreakerHasNoThresholds(t *testing.T) {
	pol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{Match: v1.ToolGuardMatch{Tool: "x"}, Breaker: &v1.BreakerSpec{Action: "off"}},
	}}
	p, err := ResolvePolicy(Tiers{Class: pol})
	require.NoError(t, err)
	r := p.RuleFor("sandbox", "x", "")
	assert.Equal(t, ActionOff, r.Action)
	assert.Zero(t, r.FailureThreshold, "off breaker must not track tool failures")
	assert.Zero(t, r.OriginFailureThreshold, "off breaker must not track origin failures")
}

func TestBuiltinDefaults(t *testing.T) {
	assert.Equal(t, int32(5), Builtin.FailureThreshold)
	assert.Equal(t, int32(10), Builtin.OriginFailureThreshold)
	assert.Equal(t, 30*time.Second, Builtin.InitialCoolOff)
	assert.Equal(t, 10*time.Minute, Builtin.MaxCoolOff)
	assert.Equal(t, ActionDeny, Builtin.Action)
	assert.False(t, Builtin.HasRateLimit())
}

func TestRuleForDataLimits(t *testing.T) {
	classPol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{
			Match:     v1.ToolGuardMatch{Tool: "dump_*"},
			DataLimit: &v1.DataLimitSpec{MaxEgressBytes: 1024, MaxIngressBytes: 4096, Action: "deny"},
		},
	}}
	p, err := ResolvePolicy(Tiers{Class: classPol})
	require.NoError(t, err)

	r := p.RuleFor("mcp", "dump_table", "mcpserver/db")
	assert.Equal(t, int64(1024), r.MaxEgressBytes)
	assert.Equal(t, int64(4096), r.MaxIngressBytes)
	assert.Equal(t, ActionDeny, r.ByteAction)
	assert.True(t, r.HasDataLimit())

	// A rule with a DataLimit but action unset defaults ByteAction to deny.
	classPol.Rules[0].DataLimit.Action = ""
	p2, err := ResolvePolicy(Tiers{Class: classPol})
	require.NoError(t, err)
	assert.Equal(t, ActionDeny, p2.RuleFor("mcp", "dump_table", "mcpserver/db").ByteAction)
}

func TestIngressLimitFor(t *testing.T) {
	cases := []struct {
		name          string
		rule          ResolvedRule
		uiDataBinding bool
		want          int64
	}{
		{name: "model path, unset: unlimited", rule: ResolvedRule{}, want: 0},
		{name: "model path, set: the configured model ceiling",
			rule: ResolvedRule{MaxIngressBytes: 1024}, want: 1024},
		{name: "UI path, nothing set: the platform browser-sized default",
			rule: ResolvedRule{}, uiDataBinding: true, want: DefaultUIIngressBytes},
		{name: "UI path never inherits the model ceiling",
			rule: ResolvedRule{MaxIngressBytes: 1024}, uiDataBinding: true,
			want: DefaultUIIngressBytes},
		{name: "UI path, configured: the configured UI ceiling wins over the default",
			rule:          ResolvedRule{MaxIngressBytes: 1024, MaxUIIngressBytes: 8192},
			uiDataBinding: true, want: 8192},
		{name: "a configured UI ceiling does not leak onto the model path",
			rule: ResolvedRule{MaxUIIngressBytes: 8192}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.rule.IngressLimitFor(tc.uiDataBinding))
		})
	}
}

// TestCeilingFloorsTheVolumeActionItBounds pins that a lower-tier rule cannot
// soften what the ToolGuardCeiling bound.
//
// AgentClass.spec.toolGuard is namespace-writable; Limits.ToolGuard is the
// cluster-admin surface the CRD calls "the Limits-side hard bound lower tiers
// cannot escape". Clamping only the MAGNITUDES left the escape wide open: a
// class rule authoring `action: warn` keeps warn through the clamp, and warn
// maps to pipeline.Decision{} in both hooks — the oversized result reaches the
// model and the over-budget call dispatches. Note the escape does not need the
// ceiling to have tightened anything: a rule whose own limit is STRICTER but
// whose action is warn passes an arbitrarily large payload just the same, so
// the floor keys off the ceiling configuring the dimension at all.
//
// deny is the floor because it is the action the ceiling already carries
// wherever it stands alone (Builtin, ungatedFallback and fromCRD's default all
// use deny). An already-stricter rule keeps its severity: halt stays halt.
func TestCeilingFloorsTheVolumeActionItBounds(t *testing.T) {
	cases := []struct {
		name           string
		rule           v1.ToolGuardRule
		ceil           *v1.ToolGuardCeiling
		wantRateAction Action
		wantByteAction Action
	}{
		{
			name: "warn dataLimit under a looser ingress ceiling: ByteAction floored to deny",
			rule: v1.ToolGuardRule{
				Match:     v1.ToolGuardMatch{Tool: "*"},
				DataLimit: &v1.DataLimitSpec{MaxIngressBytes: 1 << 30, Action: "warn"},
			},
			ceil:           &v1.ToolGuardCeiling{MaxIngressBytes: i64(2048)},
			wantRateAction: ActionDeny, wantByteAction: ActionDeny,
		},
		{
			name: "warn dataLimit STRICTER than the ceiling: still floored, since warn bounds nothing",
			rule: v1.ToolGuardRule{
				Match:     v1.ToolGuardMatch{Tool: "*"},
				DataLimit: &v1.DataLimitSpec{MaxIngressBytes: 1024, Action: "warn"},
			},
			ceil:           &v1.ToolGuardCeiling{MaxIngressBytes: i64(1 << 20)},
			wantRateAction: ActionDeny, wantByteAction: ActionDeny,
		},
		{
			name: "halt dataLimit under a byte ceiling: the stricter authored action stands",
			rule: v1.ToolGuardRule{
				Match:     v1.ToolGuardMatch{Tool: "*"},
				DataLimit: &v1.DataLimitSpec{MaxEgressBytes: 4096, Action: "halt"},
			},
			ceil:           &v1.ToolGuardCeiling{MaxEgressBytes: i64(512)},
			wantRateAction: ActionDeny, wantByteAction: ActionHalt,
		},
		{
			name: "warn dataLimit with no byte ceiling at all: the authored warn stands",
			rule: v1.ToolGuardRule{
				Match:     v1.ToolGuardMatch{Tool: "*"},
				DataLimit: &v1.DataLimitSpec{MaxIngressBytes: 1 << 30, Action: "warn"},
			},
			ceil:           &v1.ToolGuardCeiling{MaxCallsPerTurn: i32(3)},
			wantRateAction: ActionDeny, wantByteAction: ActionWarn,
		},
		{
			name: "warn rateLimit under a per-turn ceiling: RateAction floored to deny",
			rule: v1.ToolGuardRule{
				Match:     v1.ToolGuardMatch{Tool: "*"},
				RateLimit: &v1.RateLimitSpec{MaxCallsPerTurn: 50, Action: "warn"},
			},
			ceil:           &v1.ToolGuardCeiling{MaxCallsPerTurn: i32(3)},
			wantRateAction: ActionDeny, wantByteAction: ActionDeny,
		},
		{
			// A ceiling can carry its windows in RateBounds with no leading
			// pair at all (one tier authoring only rateBounds). The floor keys
			// off the bounds that will actually be enforced, not off the
			// MaxCalls/Window pointers, or the bound would resolve and warn —
			// which returns pipeline.Decision{} and dispatches the call anyway.
			name: "warn rateLimit under a ceiling expressed only in rateBounds: RateAction floored to deny",
			rule: v1.ToolGuardRule{
				Match:     v1.ToolGuardMatch{Tool: "*"},
				RateLimit: &v1.RateLimitSpec{MaxCalls: 500, Window: mdur(time.Minute), Action: "warn"},
			},
			ceil: &v1.ToolGuardCeiling{
				RateBounds: []v1.CallRateBound{{MaxCalls: 5, Window: metav1.Duration{Duration: time.Second}}},
			},
			wantRateAction: ActionDeny, wantByteAction: ActionDeny,
		},
		{
			name: "warn rateLimit under a sliding-window ceiling: RateAction floored to deny",
			rule: v1.ToolGuardRule{
				Match:     v1.ToolGuardMatch{Tool: "*"},
				RateLimit: &v1.RateLimitSpec{MaxCalls: 500, Window: mdur(time.Minute), Action: "warn"},
			},
			ceil:           &v1.ToolGuardCeiling{MaxCalls: i32(10), Window: mdur(time.Minute)},
			wantRateAction: ActionDeny, wantByteAction: ActionDeny,
		},
		{
			name: "warn rateLimit with no rate ceiling at all: the authored warn stands",
			rule: v1.ToolGuardRule{
				Match:     v1.ToolGuardMatch{Tool: "*"},
				RateLimit: &v1.RateLimitSpec{MaxCallsPerTurn: 50, Action: "warn"},
			},
			ceil:           &v1.ToolGuardCeiling{MaxIngressBytes: i64(2048)},
			wantRateAction: ActionWarn, wantByteAction: ActionDeny,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ResolvePolicy(Tiers{
				Class:   &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{tc.rule}},
				Ceiling: tc.ceil,
			})
			require.NoError(t, err)
			r := p.RuleFor("mcp", "dump_table", "mcpserver/db")
			assert.Equal(t, tc.wantRateAction, r.RateAction)
			assert.Equal(t, tc.wantByteAction, r.ByteAction)

			// The ungated meta view takes the volume clamp alone, so it must
			// carry the same floor: those tools are the ones that pull
			// third-party content into context.
			u := p.ForUngatedTools().RuleFor("mcp", "dump_table", "mcpserver/db")
			assert.Equal(t, tc.wantRateAction, u.RateAction, "the ungated view floors identically")
			assert.Equal(t, tc.wantByteAction, u.ByteAction, "the ungated view floors identically")
		})
	}
}

func TestClampAppliesUIIngressCeiling(t *testing.T) {
	ceiling := int64(4096)
	p, err := ResolvePolicy(Tiers{
		Class: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{{
			Match:     v1.ToolGuardMatch{Tool: "*"},
			DataLimit: &v1.DataLimitSpec{MaxUIIngressBytes: 1 << 20},
		}}},
		Ceiling: &v1.ToolGuardCeiling{MaxUIIngressBytes: &ceiling},
	})
	require.NoError(t, err)
	rule := p.RuleFor("mcp", "crm_list_leads", "mcpserver/crm")
	assert.Equal(t, ceiling, rule.MaxUIIngressBytes, "the ceiling min-clamps the rule")
	assert.Equal(t, ceiling, rule.IngressLimitFor(true))
}

func TestClampAppliesUIIngressCeilingWhenNoRuleConfiguresOne(t *testing.T) {
	ceiling := int64(4096)
	p, err := ResolvePolicy(Tiers{
		Ceiling: &v1.ToolGuardCeiling{MaxUIIngressBytes: &ceiling},
	})
	require.NoError(t, err)
	rule := p.RuleFor("mcp", "crm_list_leads", "mcpserver/crm")
	assert.Equal(t, ceiling, rule.IngressLimitFor(true),
		"an unset rule is 'the platform default', so a tighter ceiling must still win")
}

// TestForUngatedToolsFallback pins the two halves of the ungated-tool view's
// no-rule-matched fallback: an admin-set VOLUME ceiling reaches these tools
// (the ToolGuardCeiling CRD promises it binds "even when no lower-tier rule
// configures" one, and gated tools get exactly that), while Builtin's breaker
// never does at any severity — a consecutive-failure breaker calibrated for
// external dependencies would wedge the session by denying agent_work_complete.
func TestForUngatedToolsFallback(t *testing.T) {
	cases := []struct {
		name  string
		ceil  *v1.ToolGuardCeiling
		check func(t *testing.T, r ResolvedRule)
	}{
		{
			name: "no ceiling: rule is entirely inert, so nothing guards the tool",
			ceil: nil,
			check: func(t *testing.T, r ResolvedRule) {
				assert.True(t, r.Disabled(), "no rule and no ceiling = nothing to enforce")
			},
		},
		{
			name: "byte ceiling: imposed on the fallback, with no breaker",
			ceil: &v1.ToolGuardCeiling{MaxEgressBytes: i64(512), MaxIngressBytes: i64(2048)},
			check: func(t *testing.T, r ResolvedRule) {
				assert.Equal(t, int64(512), r.MaxEgressBytes)
				assert.Equal(t, int64(2048), r.MaxIngressBytes)
				assert.Equal(t, ActionDeny, r.ByteAction)
				assert.False(t, r.Disabled(), "a byte ceiling makes the rule live")
				assert.Zero(t, r.FailureThreshold, "the ceiling must not bring a breaker with it")
			},
		},
		{
			name: "rate ceiling: imposed on the fallback, with no breaker",
			ceil: &v1.ToolGuardCeiling{MaxCallsPerTurn: i32(3), MaxCalls: i32(10), Window: mdur(time.Minute)},
			check: func(t *testing.T, r ResolvedRule) {
				assert.Equal(t, int32(3), r.RateMaxPerTurn)
				assert.Equal(t, int32(10), r.RateMaxCalls)
				assert.Equal(t, time.Minute, r.RateWindow)
				assert.Equal(t, ActionDeny, r.RateAction)
				assert.Zero(t, r.FailureThreshold, "the ceiling must not bring a breaker with it")
			},
		},
		{
			name: "breaker-knobs-only ceiling: minAction must not re-arm Builtin's breaker",
			ceil: &v1.ToolGuardCeiling{MinAction: sp("halt"), MaxFailureThreshold: i32(2), MinInitialCoolOff: mdur(time.Minute)},
			check: func(t *testing.T, r ResolvedRule) {
				assert.Equal(t, ActionOff, r.Action, "MinAction floors a RULE's action; there is no rule here")
				assert.Zero(t, r.FailureThreshold)
				assert.Zero(t, r.OriginFailureThreshold)
				assert.True(t, r.Disabled(), "breaker knobs alone leave an ungated tool unguarded")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ResolvePolicy(Tiers{Ceiling: tc.ceil})
			require.NoError(t, err)
			tc.check(t, p.ForUngatedTools().RuleFor("meta", "query_memory", ""))

			// The gated walk is untouched: it still falls back to Builtin.
			assert.Equal(t, "builtin", p.RuleFor("meta", "query_memory", "").Provenance,
				"the ungated view must not change what gated tools resolve to")
		})
	}
}

// TestForUngatedToolsHonorsAuthoredRules pins the other half: the view changes
// only the fallback. A rule an admin wrote — breaker included — resolves
// identically for ungated and gated tools, so a rule targeting kind "meta" is
// not quietly weakened by the Builtin carve-out.
func TestForUngatedToolsHonorsAuthoredRules(t *testing.T) {
	pol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{
			Match:   v1.ToolGuardMatch{Kind: "meta", Tool: "query_*"},
			Breaker: &v1.BreakerSpec{FailureThreshold: 2, Action: "deny"},
		},
	}}
	p, err := ResolvePolicy(Tiers{Class: pol, Ceiling: &v1.ToolGuardCeiling{MaxIngressBytes: i64(4096)}})
	require.NoError(t, err)

	ungated := p.ForUngatedTools().RuleFor("meta", "query_memory", "")
	assert.Equal(t, p.RuleFor("meta", "query_memory", ""), ungated,
		"a matched rule resolves the same either way")
	assert.Equal(t, int32(2), ungated.FailureThreshold, "an AUTHORED breaker still applies")
	assert.Equal(t, int64(4096), ungated.MaxIngressBytes, "the ceiling still clamps a matched rule")

	// A name the rule does not match falls through to the breakerless fallback.
	unmatched := p.ForUngatedTools().RuleFor("meta", "agent_work_complete", "")
	assert.Zero(t, unmatched.FailureThreshold, "an unmatched ungated tool gets no breaker")
	assert.Equal(t, int64(4096), unmatched.MaxIngressBytes, "but the ceiling still binds it")
}

func TestForUngatedToolsIsStableAndNilSafe(t *testing.T) {
	p, err := ResolvePolicy(Tiers{})
	require.NoError(t, err)
	v := p.ForUngatedTools()
	require.NotNil(t, v)
	assert.Same(t, v, p.ForUngatedTools(), "the view is built once, not per call")
	assert.Same(t, v, v.ForUngatedTools(), "the view of the view is itself")

	var nilPolicy *ResolvedPolicy
	assert.Nil(t, nilPolicy.ForUngatedTools(), "a session with no policy stays nil, not a panic")
}

func TestClampImposesByteCeilings(t *testing.T) {
	ceiling := &v1.ToolGuardCeiling{MaxEgressBytes: i64(512), MaxIngressBytes: i64(2048)}

	// No lower-tier data limit → ceiling imposes one, backed by builtin ByteAction.
	p, err := ResolvePolicy(Tiers{Ceiling: ceiling})
	require.NoError(t, err)
	r := p.RuleFor("sandbox", "anytool", "")
	assert.Equal(t, int64(512), r.MaxEgressBytes, "ceiling imposes egress even with no rule")
	assert.Equal(t, int64(2048), r.MaxIngressBytes)
	assert.Equal(t, ActionDeny, r.ByteAction)

	// Lower-tier limit looser than the ceiling → min wins; rule action preserved.
	classPol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{Match: v1.ToolGuardMatch{}, DataLimit: &v1.DataLimitSpec{MaxEgressBytes: 1 << 20, Action: "warn"}},
	}}
	p2, err := ResolvePolicy(Tiers{Class: classPol, Ceiling: ceiling})
	require.NoError(t, err)
	r2 := p2.RuleFor("sandbox", "x", "")
	assert.Equal(t, int64(512), r2.MaxEgressBytes, "ceiling clamps the looser rule limit")
	assert.Equal(t, int64(2048), r2.MaxIngressBytes, "ceiling imposes ingress the rule omitted")
	// This assertion used to read "rule action preserved" and expect ActionWarn.
	// It pinned the escape: warn maps to pipeline.Decision{}, so the clamped
	// 512/2048 bounds enforced nothing and a namespace-writable rule neutered
	// the cluster admin's ceiling. See floorVolumeActions.
	assert.Equal(t, ActionDeny, r2.ByteAction, "a rule cannot soften a dimension the ceiling bounds")
}

// TestClampVolume_RateCeilingNeverRelaxesAnAuthoredRule pins that the ceiling's
// rate clamp is a CEILING: maxCalls and window only mean anything together, so
// comparing the call COUNTS across two different windows can pick the looser
// pair — a {100 calls, 1m} burst ceiling is 144,000/day and must never replace
// an authored {200 calls, 24h}. ToolGuardCeiling's contract is strictest-wins.
//
// wantCeilings is the other half of that contract: when the windows differ,
// neither bound implies the other, so the ceiling's is carried alongside the
// authored pair (RateCeilings) instead of one of them being dropped. An equal
// window still collapses to min(maxCalls), which loses nothing.
func TestClampVolume_RateCeilingNeverRelaxesAnAuthoredRule(t *testing.T) {
	// rate returns calls/second, the only comparable quantity across windows.
	rate := func(maxCalls int32, window time.Duration) float64 {
		if maxCalls <= 0 || window <= 0 {
			return math.Inf(1) // unenforceable ⇒ unlimited
		}
		return float64(maxCalls) / window.Seconds()
	}

	cases := []struct {
		name           string
		rule           *v1.RateLimitSpec
		ceil           *v1.ToolGuardCeiling
		wantMaxCalls   int32
		wantWindow     time.Duration
		wantCeilings   []WindowLimit
		wantRateAction Action
	}{
		{
			name:         "ceiling rate looser on average but a tighter burst: authored pair survives AND the burst binds",
			rule:         &v1.RateLimitSpec{MaxCalls: 200, Window: mdur(24 * time.Hour), Action: "deny"},
			ceil:         &v1.ToolGuardCeiling{MaxCalls: i32(100), Window: mdur(time.Minute)},
			wantMaxCalls: 200, wantWindow: 24 * time.Hour, wantRateAction: ActionDeny,
			wantCeilings: []WindowLimit{{MaxCalls: 100, Window: time.Minute}},
		},
		{
			// A ceiling folded from two tiers that named different windows
			// carries both (ToolGuardCeiling.RateBounds). Neither may be traded
			// away by an authored rule on a third window.
			name: "ceiling carries two windows of its own: both bind alongside the authored pair",
			rule: &v1.RateLimitSpec{MaxCalls: 200, Window: mdur(24 * time.Hour), Action: "deny"},
			ceil: &v1.ToolGuardCeiling{
				MaxCalls: i32(100), Window: mdur(time.Minute),
				RateBounds: []v1.CallRateBound{{MaxCalls: 5, Window: metav1.Duration{Duration: time.Second}}},
			},
			wantMaxCalls: 200, wantWindow: 24 * time.Hour, wantRateAction: ActionDeny,
			wantCeilings: []WindowLimit{
				{MaxCalls: 100, Window: time.Minute},
				{MaxCalls: 5, Window: time.Second},
			},
		},
		{
			// The rule's own window matches one ceiling bound and not the other:
			// the matching one collapses to min(maxCalls), the other is carried.
			name: "ceiling bound on the authored window collapses; the other is carried",
			rule: &v1.RateLimitSpec{MaxCalls: 90, Window: mdur(time.Minute), Action: "deny"},
			ceil: &v1.ToolGuardCeiling{
				MaxCalls: i32(30), Window: mdur(time.Minute),
				RateBounds: []v1.CallRateBound{{MaxCalls: 2, Window: metav1.Duration{Duration: time.Second}}},
			},
			wantMaxCalls: 30, wantWindow: time.Minute, wantRateAction: ActionDeny,
			wantCeilings: []WindowLimit{{MaxCalls: 2, Window: time.Second}},
		},
		{
			name:         "ceiling rate stricter: the pair is replaced together",
			rule:         &v1.RateLimitSpec{MaxCalls: 200, Window: mdur(24 * time.Hour), Action: "deny"},
			ceil:         &v1.ToolGuardCeiling{MaxCalls: i32(10), Window: mdur(24 * time.Hour)},
			wantMaxCalls: 10, wantWindow: 24 * time.Hour, wantRateAction: ActionDeny,
		},
		{
			name: "rule sets no sliding-window limit: the ceiling imposes one, at deny",
			rule: &v1.RateLimitSpec{MaxCallsPerTurn: 4, Action: "warn"},
			ceil: &v1.ToolGuardCeiling{MaxCalls: i32(100), Window: mdur(time.Minute)},
			// This row expected ActionWarn, which made the imposed 100/1m inert:
			// warn returns pipeline.Decision{} and the call dispatches anyway.
			// See floorVolumeActions.
			wantMaxCalls: 100, wantWindow: time.Minute, wantRateAction: ActionDeny,
		},
		{
			name:         "rule sets maxCalls with no window (enforces nothing): the ceiling imposes its pair",
			rule:         &v1.RateLimitSpec{MaxCalls: 50, Action: "deny"},
			ceil:         &v1.ToolGuardCeiling{MaxCalls: i32(100), Window: mdur(time.Minute)},
			wantMaxCalls: 100, wantWindow: time.Minute, wantRateAction: ActionDeny,
		},
		{
			// Equal average rates, different shapes: the authored 60/1m stands
			// (the ceiling does not re-author it into a per-second drip) and the
			// admin's 1/1s burst bound binds too, so 60 calls in one second —
			// which the authored pair alone permits — is denied.
			name:         "equal average rates, different windows: the authored shape stands and the burst still binds",
			rule:         &v1.RateLimitSpec{MaxCalls: 60, Window: mdur(time.Minute), Action: "deny"},
			ceil:         &v1.ToolGuardCeiling{MaxCalls: i32(1), Window: mdur(time.Second)},
			wantMaxCalls: 60, wantWindow: time.Minute, wantRateAction: ActionDeny,
			wantCeilings: []WindowLimit{{MaxCalls: 1, Window: time.Second}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
				{Match: v1.ToolGuardMatch{Tool: "*"}, RateLimit: tc.rule},
			}}
			p, err := ResolvePolicy(Tiers{Class: pol, Ceiling: tc.ceil})
			require.NoError(t, err)
			r := p.RuleFor("sandbox", "anytool", "")

			assert.Equal(t, tc.wantMaxCalls, r.RateMaxCalls)
			assert.Equal(t, tc.wantWindow, r.RateWindow)
			assert.Equal(t, tc.wantCeilings, r.RateCeilings)
			// The ceiling clamps the rule's volume MAGNITUDES downward and floors
			// its action upward (never below deny, never below what the rule
			// authored) — see TestCeilingFloorsTheVolumeActionItBounds.
			assert.Equal(t, tc.wantRateAction, r.RateAction)
			assert.LessOrEqual(t, rate(r.RateMaxCalls, r.RateWindow), rate(tc.rule.MaxCalls, ruleWindow(tc.rule)),
				"the resolved rate must never exceed the authored one")
		})
	}
}

// TestCeilingBurstBoundBindsAlongsideALongerAuthoredWindow pins that a ceiling
// authored as a BURST bound is not discarded by a lower tier that authors a
// longer window with a lower average rate.
//
// Comparing the two as one calls/second scalar can keep only the pair that wins
// that comparison, and neither pair implies the other at every horizon: a
// ceiling of {100, 1m} permits 144,000/day (so it cannot stand in for the
// authored {200, 24h}), while {200, 24h} permits all 200 inside one second (so
// it cannot stand in for the admin's per-minute burst bound). Both are real
// bounds their author wrote, so the resolved rule carries both and a call is
// admitted only when EVERY window has room.
func TestCeilingBurstBoundBindsAlongsideALongerAuthoredWindow(t *testing.T) {
	p, err := ResolvePolicy(Tiers{
		Class: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{{
			Match:     v1.ToolGuardMatch{Tool: "*"},
			RateLimit: &v1.RateLimitSpec{MaxCalls: 200, Window: mdur(24 * time.Hour), Action: "deny"},
		}}},
		Ceiling: &v1.ToolGuardCeiling{MaxCalls: i32(100), Window: mdur(time.Minute)},
	})
	require.NoError(t, err)

	r := p.RuleFor("mcp", "dump_table", "mcpserver/db")
	assert.Equal(t, int32(200), r.RateMaxCalls, "the authored daily budget is not overwritten")
	assert.Equal(t, 24*time.Hour, r.RateWindow)
	assert.Equal(t, []WindowLimit{{MaxCalls: 100, Window: time.Minute}}, r.RateCeilings,
		"the admin's burst bound is carried alongside it")

	// Enforcement: 150 calls in the same instant. The authored 200/24h has room
	// for all of them; the admin's 100/1m does not.
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	admitted, denied := 0, Admission{}
	for i := 0; i < 150; i++ {
		adm, _ := reg.Admit(context.Background(), ToolKey("dump_table"), "", r, 0)
		if adm.Allowed {
			admitted++
			continue
		}
		if denied.DeniedBy == "" {
			denied = adm
		}
	}
	assert.Equal(t, 100, admitted, "the ceiling's 100/minute must bind, not only the authored 200/24h")
	assert.Equal(t, "rate_window", denied.DeniedBy)
	assert.Equal(t, WindowLimit{MaxCalls: 100, Window: time.Minute}, denied.RateLimit,
		"the denial names the bound that actually fired, so the model is not told the wrong budget")

	// A minute later the burst window has drained, but the daily budget has not:
	// the remaining 100 of the authored 200 are admitted and the 201st is not.
	clk.t = clk.t.Add(2 * time.Minute)
	admitted = 0
	for i := 0; i < 150; i++ {
		if adm, _ := reg.Admit(context.Background(), ToolKey("dump_table"), "", r, 0); adm.Allowed {
			admitted++
		}
	}
	assert.Equal(t, 100, admitted, "the authored 200/24h still binds once the burst window drains")
}

// TestAdmitSweepsEveryCeilingWindow pins the enforcement half of a ceiling that
// names MORE than one window — the shape pkg/platform/settings produces when the cluster
// and namespace tiers each wrote a bound and the windows differ. All of them
// have to gate: with an authored 100/hour under an admin 10/minute + 3/second,
// a caller is stopped by the second bound first, then by the minute bound, and
// call history is kept for the longest window so the hourly budget still counts
// everything that came before.
func TestAdmitSweepsEveryCeilingWindow(t *testing.T) {
	p, err := ResolvePolicy(Tiers{
		Class: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{{
			Match:     v1.ToolGuardMatch{Tool: "*"},
			RateLimit: &v1.RateLimitSpec{MaxCalls: 100, Window: mdur(time.Hour), Action: "deny"},
		}}},
		Ceiling: &v1.ToolGuardCeiling{
			MaxCalls: i32(10), Window: mdur(time.Minute),
			RateBounds: []v1.CallRateBound{{MaxCalls: 3, Window: metav1.Duration{Duration: time.Second}}},
		},
	})
	require.NoError(t, err)
	r := p.RuleFor("mcp", "fetch", "mcpserver/api")
	require.Equal(t, 100, int(r.RateMaxCalls), "the authored hourly budget is untouched")
	assert.Equal(t, time.Hour, r.longestWindow(), "history must span the longest enforced bound")

	clk := &fakeClock{t: time.Unix(2000, 0)}
	reg := NewRegistry(clk.now)
	burst := func(n int) (int, Admission) {
		admitted, denied := 0, Admission{}
		for i := 0; i < n; i++ {
			adm, _ := reg.Admit(context.Background(), ToolKey("fetch"), OriginKey("mcpserver/api"), r, 0)
			if adm.Allowed {
				admitted++
				continue
			}
			if denied.DeniedBy == "" {
				denied = adm
			}
		}
		return admitted, denied
	}

	admitted, denied := burst(9)
	assert.Equal(t, 3, admitted, "the per-second bound stops a burst the minute and hour bounds have room for")
	assert.Equal(t, WindowLimit{MaxCalls: 3, Window: time.Second}, denied.RateLimit)

	// Space the calls out: each round drains the second-window bound, and the
	// minute bound takes over once ten have landed inside it.
	total := admitted
	for round := 0; round < 4; round++ {
		clk.t = clk.t.Add(2 * time.Second)
		n, d := burst(9)
		total += n
		denied = d
	}
	assert.Equal(t, 10, total, "the admin's 10/minute binds even though the authored 100/hour has room")
	assert.Equal(t, WindowLimit{MaxCalls: 10, Window: time.Minute}, denied.RateLimit,
		"the denial names the bound that fired, not whichever was checked first")
}

// ruleWindow is the authored window as a plain Duration (0 when unset).
func ruleWindow(rl *v1.RateLimitSpec) time.Duration {
	if rl == nil || rl.Window == nil {
		return 0
	}
	return rl.Window.Duration
}
