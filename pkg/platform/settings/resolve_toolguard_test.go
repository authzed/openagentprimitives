package settings

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func durPtr(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }
func i32Ptr(v int32) *int32                   { return &v }

func TestResolveToolGuardFoldsCeilingsAndCarriesPolicies(t *testing.T) {
	clusterPol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{{Match: v1.ToolGuardMatch{Kind: "mcp"}}}}
	nsPol := &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{{Match: v1.ToolGuardMatch{Tool: "x_*"}}}}

	in := Inputs{
		Cluster: &v1.SettingsSpec{
			Limits: &v1.SettingsLimits{ToolGuard: &v1.ToolGuardCeiling{
				MaxFailureThreshold: i32Ptr(20),
				MinInitialCoolOff:   durPtr(10 * time.Second),
				MinAction:           strPtr("warn"),
			}},
			Defaults: &v1.SettingsDefaults{ToolGuard: clusterPol},
		},
		Namespace: &v1.SettingsSpec{
			Limits: &v1.SettingsLimits{ToolGuard: &v1.ToolGuardCeiling{
				MaxFailureThreshold: i32Ptr(8),
				MinInitialCoolOff:   durPtr(time.Minute),
				MinAction:           strPtr("halt"),
				MaxCallsPerTurn:     i32Ptr(40),
			}},
			Defaults: &v1.SettingsDefaults{ToolGuard: nsPol},
		},
	}
	eff, vs := Resolve(in)
	assert.Empty(t, fatalOf(vs))
	require.NotNil(t, eff.ToolGuard)
	// Per-tier policies pass through unfolded.
	assert.Equal(t, clusterPol, eff.ToolGuard.Cluster)
	assert.Equal(t, nsPol, eff.ToolGuard.Namespace)
	// Ceilings folded strictest: min threshold, max cool-off floor, max severity.
	require.NotNil(t, eff.ToolGuard.Ceiling)
	assert.Equal(t, int32(8), *eff.ToolGuard.Ceiling.MaxFailureThreshold)
	assert.Equal(t, time.Minute, eff.ToolGuard.Ceiling.MinInitialCoolOff.Duration)
	assert.Equal(t, "halt", *eff.ToolGuard.Ceiling.MinAction)
	assert.Equal(t, int32(40), *eff.ToolGuard.Ceiling.MaxCallsPerTurn)
}

func TestResolveToolGuardNilWhenNoTierSetsAnything(t *testing.T) {
	eff, _ := Resolve(Inputs{})
	assert.Nil(t, eff.ToolGuard, "no config anywhere → nil (runner still applies Builtin)")
}

func fatalOf(vs []Violation) []Violation {
	var out []Violation
	for _, v := range vs {
		if v.Fatal {
			out = append(out, v)
		}
	}
	return out
}

// bound is one sliding-window bound in the compact form these tests compare.
type bound struct {
	maxCalls int32
	window   time.Duration
}

// boundsOf reads every sliding-window bound a folded ceiling carries: the
// leading pair first, then RateBounds in their stored order.
func boundsOf(t *testing.T, c *v1.ToolGuardCeiling) []bound {
	t.Helper()
	require.NotNil(t, c)
	var out []bound
	if c.MaxCalls != nil && c.Window != nil {
		out = append(out, bound{*c.MaxCalls, c.Window.Duration})
	}
	for _, b := range c.RateBounds {
		out = append(out, bound{b.MaxCalls, b.Window.Duration})
	}
	return out
}

// TestFoldToolGuardCeiling_KeepsEveryTiersBound pins that folding two tiers'
// rate ceilings is a CONJUNCTION, not a choice: maxCalls and window mean nothing
// apart, so no single pair can stand in for two different windows — a cluster
// {5, 1s} permits 432,000/day and a namespace {10, 24h} permits all 10 inside
// one second. Picking either by calls/second silently deletes the other admin's
// ceiling on the surface the CRD calls the hard bound lower tiers cannot escape.
// Every distinct window must survive, with the strictest-by-rate pair LEADING.
func TestFoldToolGuardCeiling_KeepsEveryTiersBound(t *testing.T) {
	// rate returns calls/second — the only comparable quantity across windows.
	rate := func(b bound) float64 { return v1.CallRate(b.maxCalls, b.window) }

	cases := []struct {
		name        string
		cluster     *v1.ToolGuardCeiling
		namespace   *v1.ToolGuardCeiling
		wantBounds  []bound
		wantReports int
	}{
		{
			name:       "differing windows: the lower average rate leads and the burst bound survives beside it",
			cluster:    &v1.ToolGuardCeiling{MaxCalls: i32Ptr(5), Window: durPtr(time.Second)},
			namespace:  &v1.ToolGuardCeiling{MaxCalls: i32Ptr(10), Window: durPtr(24 * time.Hour)},
			wantBounds: []bound{{10, 24 * time.Hour}, {5, time.Second}},
		},
		{
			name:       "equal windows collapse to the stricter count, whichever order they fold in",
			cluster:    &v1.ToolGuardCeiling{MaxCalls: i32Ptr(100), Window: durPtr(time.Hour)},
			namespace:  &v1.ToolGuardCeiling{MaxCalls: i32Ptr(10), Window: durPtr(time.Hour)},
			wantBounds: []bound{{10, time.Hour}},
		},
		{
			name:       "only one tier sets a rate ceiling: it is carried through alone",
			cluster:    &v1.ToolGuardCeiling{MaxCalls: i32Ptr(7), Window: durPtr(time.Minute)},
			namespace:  &v1.ToolGuardCeiling{MaxFailureThreshold: i32Ptr(2)},
			wantBounds: []bound{{7, time.Minute}},
		},
		{
			name:        "a half-authored tier (maxCalls, no window) enforces nothing: reported, and does not displace a real bound",
			cluster:     &v1.ToolGuardCeiling{MaxCalls: i32Ptr(1)},
			namespace:   &v1.ToolGuardCeiling{MaxCalls: i32Ptr(10), Window: durPtr(time.Hour)},
			wantBounds:  []bound{{10, time.Hour}},
			wantReports: 1,
		},
		{
			name: "a tier authoring burst-plus-sustained itself: all three windows survive, ordered",
			cluster: &v1.ToolGuardCeiling{
				MaxCalls: i32Ptr(5), Window: durPtr(time.Second),
				RateBounds: []v1.CallRateBound{{MaxCalls: 100, Window: metav1.Duration{Duration: time.Minute}}},
			},
			namespace:  &v1.ToolGuardCeiling{MaxCalls: i32Ptr(10), Window: durPtr(24 * time.Hour)},
			wantBounds: []bound{{10, 24 * time.Hour}, {5, time.Second}, {100, time.Minute}},
		},
		{
			name:      "the same window on both sides of the pair/RateBounds split still collapses to the min",
			cluster:   &v1.ToolGuardCeiling{MaxCalls: i32Ptr(50), Window: durPtr(time.Minute)},
			namespace: &v1.ToolGuardCeiling{RateBounds: []v1.CallRateBound{{MaxCalls: 20, Window: metav1.Duration{Duration: time.Minute}}}},
			// One window authored twice is one bound, at the stricter count —
			// carrying both would cost a second sweep in Admit for nothing.
			wantBounds: []bound{{20, time.Minute}},
		},
		{
			name:        "a rateBounds entry with a zero window enforces nothing: reported, not folded in",
			cluster:     &v1.ToolGuardCeiling{RateBounds: []v1.CallRateBound{{MaxCalls: 9}}},
			namespace:   &v1.ToolGuardCeiling{MaxCalls: i32Ptr(10), Window: durPtr(time.Hour)},
			wantBounds:  []bound{{10, time.Hour}},
			wantReports: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, vs := foldToolGuardCeiling(tc.cluster, tc.namespace)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantBounds, boundsOf(t, got))
			assert.Empty(t, fatalOf(vs), "an unenforceable bound is a warning, never a session-blocking error")
			assert.Len(t, vs, tc.wantReports, "every dropped bound must be reported")
			for _, v := range vs {
				assert.Equal(t, ReasonToolGuardRateBoundUnenforceable, v.Reason)
			}

			// Whatever the shapes, no tier's enforceable bound may be relaxed or
			// lost: each one must appear in the fold at no higher a count.
			for _, tier := range []*v1.ToolGuardCeiling{tc.cluster, tc.namespace} {
				enforceable, _ := tierRateBounds(tier)
				for _, want := range enforceable {
					found := false
					for _, got := range boundsOf(t, got) {
						if got.window == want.Window.Duration && got.maxCalls <= want.MaxCalls {
							found = true
						}
					}
					assert.True(t, found, "tier bound %d/%s must survive the fold", want.MaxCalls, want.Window.Duration)
				}
			}
			// The leading pair is still the strictest on average, so a consumer
			// that reads only maxCalls/window sees a real bound.
			all := boundsOf(t, got)
			for _, b := range all[1:] {
				assert.LessOrEqual(t, rate(all[0]), rate(b), "the strictest-by-rate bound leads")
			}
		})
	}
}
