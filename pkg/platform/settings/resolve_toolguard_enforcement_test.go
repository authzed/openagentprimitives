// pkg/platform/settings/resolve_toolguard_enforcement_test.go
//
// The cross-tier ceiling fold is only worth what the enforcement path binds on,
// so these tests run the whole span: Resolve folds two tiers' Limits.ToolGuard,
// pkg/authz/toolguard resolves the fold into a rule, and Registry.Admit admits
// calls against it. A fold that quietly kept one tier's bound is invisible from
// either side alone — this package sees a well-formed ceiling, toolguard sees a
// well-formed rule — and shows up only here.
package settings

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
)

// ceilingTiers builds Inputs whose cluster and namespace tiers carry only the
// given tool-guard ceilings.
func ceilingTiers(t *testing.T, cluster, namespace *v1.ToolGuardCeiling) Inputs {
	t.Helper()
	mk := func(c *v1.ToolGuardCeiling) *v1.SettingsSpec {
		if c == nil {
			return nil
		}
		return &v1.SettingsSpec{Limits: &v1.SettingsLimits{ToolGuard: c}}
	}
	return Inputs{Cluster: mk(cluster), Namespace: mk(namespace)}
}

// admitBurst resolves the folded ceiling into a rule and admits calls at one
// frozen instant until one is denied, returning how many got through and the
// denial. Every call shares a timestamp, so only a sub-second window can deny.
func admitBurst(t *testing.T, ceiling *v1.ToolGuardCeiling, attempts int) (int, toolguard.Admission) {
	t.Helper()
	pol, err := toolguard.ResolvePolicy(toolguard.Tiers{Ceiling: ceiling})
	require.NoError(t, err, "ResolvePolicy over the folded ceiling")
	rule := pol.RuleFor("mcp", "lookup", "mcpserver/demo")

	now := time.Now()
	reg := toolguard.NewRegistry(func() time.Time { return now })
	admitted := 0
	for i := 0; i < attempts; i++ {
		adm, _ := reg.Admit(context.Background(),
			toolguard.ToolKey("lookup"), toolguard.OriginKey("mcpserver/demo"), rule, 0)
		if !adm.Allowed {
			return admitted, adm
		}
		admitted++
	}
	return admitted, toolguard.Admission{Allowed: true}
}

// TestFoldedCeilingEnforcesEveryTiersWindow pins the cross-tier half of the
// bound-conjunction rule pkg/authz/toolguard applies per rule: two tiers naming
// different windows have written two bounds, and a call must fit under BOTH.
// Collapsing them to one calls/second scalar drops whichever bound the other
// tier's admin wrote — a cluster burst ceiling vanishing behind a namespace
// daily budget, backwards for the surface the CRD calls "the hard bound lower
// tiers cannot escape".
func TestFoldedCeilingEnforcesEveryTiersWindow(t *testing.T) {
	cases := []struct {
		name          string
		cluster       *v1.ToolGuardCeiling
		namespace     *v1.ToolGuardCeiling
		wantAdmitted  int
		wantRateLimit toolguard.WindowLimit
	}{
		{
			// The namespace budget wins a calls/second comparison (10/day is far
			// below 5/s); keeping it alone would admit all ten in one instant —
			// exactly the burst the cluster admin bounded.
			name:          "cluster burst under a namespace daily budget: the burst bound still denies",
			cluster:       &v1.ToolGuardCeiling{MaxCalls: i32Ptr(5), Window: durPtr(time.Second)},
			namespace:     &v1.ToolGuardCeiling{MaxCalls: i32Ptr(10), Window: durPtr(24 * time.Hour)},
			wantAdmitted:  5,
			wantRateLimit: toolguard.WindowLimit{MaxCalls: 5, Window: time.Second},
		},
		{
			// Mirror image: the tier writing the burst bound is the namespace.
			name:          "namespace burst under a cluster daily budget: the burst bound still denies",
			cluster:       &v1.ToolGuardCeiling{MaxCalls: i32Ptr(10), Window: durPtr(24 * time.Hour)},
			namespace:     &v1.ToolGuardCeiling{MaxCalls: i32Ptr(3), Window: durPtr(time.Second)},
			wantAdmitted:  3,
			wantRateLimit: toolguard.WindowLimit{MaxCalls: 3, Window: time.Second},
		},
		{
			// Same window on both sides still collapses to min(maxCalls): one
			// bound expresses both exactly, so nothing is carried alongside.
			name:          "equal windows collapse to the stricter count",
			cluster:       &v1.ToolGuardCeiling{MaxCalls: i32Ptr(4), Window: durPtr(time.Second)},
			namespace:     &v1.ToolGuardCeiling{MaxCalls: i32Ptr(9), Window: durPtr(time.Second)},
			wantAdmitted:  4,
			wantRateLimit: toolguard.WindowLimit{MaxCalls: 4, Window: time.Second},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eff, vs := Resolve(ceilingTiers(t, tc.cluster, tc.namespace))
			assert.Empty(t, fatalOf(vs))
			require.NotNil(t, eff.ToolGuard)
			require.NotNil(t, eff.ToolGuard.Ceiling, "both tiers set a ceiling")

			admitted, denial := admitBurst(t, eff.ToolGuard.Ceiling, 12)
			assert.Equal(t, tc.wantAdmitted, admitted,
				"every tier's sliding-window bound must bind; a call has to fit under all of them")
			assert.Equal(t, "rate_window", denial.DeniedBy)
			assert.Equal(t, tc.wantRateLimit, denial.RateLimit,
				"the denial must quote the bound that actually fired")
		})
	}
}
