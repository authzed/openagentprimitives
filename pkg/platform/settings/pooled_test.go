package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func budget(turns int32, tokens int64) *v1.BudgetConfig {
	return &v1.BudgetConfig{
		MaxTurns: turns, MaxTokens: tokens,
		MaxDuration: metav1.Duration{Duration: 0},
	}
}

// agentBudget builds a BudgetConfig carrying only MaxDelegatedAgents, for the
// pooled-per-root-ceiling fold tests below.
func agentBudget(n int32) *v1.BudgetConfig {
	return &v1.BudgetConfig{MaxDelegatedAgents: n}
}

func TestResolve_RootBudgetCapsAChild(t *testing.T) {
	cases := []struct {
		name       string
		class      *v1.BudgetConfig
		root       *v1.BudgetConfig
		wantTurns  int32
		wantTokens int64
	}{
		{
			name:  "no root budget: class value stands (root session)",
			class: budget(50, 100000), root: nil,
			wantTurns: 50, wantTokens: 100000,
		},
		{
			name:  "root narrower: root wins on both dimensions",
			class: budget(50, 100000), root: budget(10, 20000),
			wantTurns: 10, wantTokens: 20000,
		},
		{
			name:  "root wider: class still wins (a ceiling only narrows)",
			class: budget(10, 20000), root: budget(50, 100000),
			wantTurns: 10, wantTokens: 20000,
		},
		{
			name:  "mixed: each dimension folds independently",
			class: budget(50, 20000), root: budget(10, 100000),
			wantTurns: 10, wantTokens: 20000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Resolve(Inputs{ClassBudget: tc.class, RootBudget: tc.root})
			assert.Equal(t, tc.wantTurns, got.Budget.MaxTurns)
			assert.Equal(t, tc.wantTokens, got.Budget.MaxTokens)
		})
	}
}

// TestResolve_MaxDelegatedAgents proves the pooled per-root total-agent
// ceiling fold: class request → tier defaults → tier ceilings, exactly like
// every sibling budget dimension, EXCEPT that it is never capped by
// RootBudget — the last case is the one that fails if a
// ceilDim(in.RootBudget, ...) arm is added back to the agents fold in
// resolveBudget.
//
// That row deliberately makes the class value LARGER than the root's: a
// ceiling only ever narrows (resolveDim takes the MINIMUM of every ceiling
// in play), and the class's own value already contributes its own ceiling
// via ceilDim(in.ClassBudget, ...). If the class's ceiling were instead the
// smaller of the two, a re-added root arm would compute
// min(classCeiling, rootCeiling) == classCeiling either way — same result
// whether the root arm exists or not, so the row would pass regardless of
// the bug it claims to catch. Making the root value the SMALLER one is what
// makes a re-added root arm actually change the answer.
func TestResolve_MaxDelegatedAgents(t *testing.T) {
	cases := []struct {
		name    string
		class   *v1.BudgetConfig
		ns      *v1.SettingsSpec
		cluster *v1.SettingsSpec
		root    *v1.BudgetConfig
		want    int32
		// wantClampedDim, when non-empty, asserts exactly one BudgetClamped
		// violation whose message names this dimension.
		wantClampedDim string
	}{
		{
			name:  "class declares 4, no tiers: resolves to 4",
			class: agentBudget(4),
			want:  4,
		},
		{
			name:  "class declares 0 (unset), namespace default 8: resolves to 8",
			class: agentBudget(0),
			ns:    &v1.SettingsSpec{Defaults: &v1.SettingsDefaults{Budget: agentBudget(8)}},
			want:  8,
		},
		{
			name:           "class declares 20, cluster ceiling 8: resolves to 8, BudgetClamped names maxDelegatedAgents",
			class:          agentBudget(20),
			cluster:        &v1.SettingsSpec{Limits: &v1.SettingsLimits{Budget: &v1.SettingsBudgetCeiling{MaxDelegatedAgents: 8}}},
			want:           8,
			wantClampedDim: "maxDelegatedAgents",
		},
		{
			name:  "class declares 20, root declares 4 (smaller): resolves to 20 — the root arm is deliberately absent, so the root's narrower value does not clamp the class's",
			class: agentBudget(20),
			root:  agentBudget(4),
			want:  20,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, vs := Resolve(Inputs{ClassBudget: tc.class, Namespace: tc.ns, Cluster: tc.cluster, RootBudget: tc.root})
			assert.Equal(t, tc.want, got.Budget.MaxDelegatedAgents)
			if tc.wantClampedDim == "" {
				assert.Empty(t, vs)
				return
			}
			require.Len(t, vs, 1)
			assert.Equal(t, ReasonBudgetClamped, vs[0].Reason)
			assert.Contains(t, vs[0].Message, tc.wantClampedDim)
		})
	}
}
