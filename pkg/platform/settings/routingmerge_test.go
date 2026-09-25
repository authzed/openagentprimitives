package settings

import (
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
)

// TestMergeRouting exercises the narrowing-only merge rules: an AgentClass's
// RoutingMetadata (overlay) refines the catalog-resolved OpenRouterRouting
// (base), but can only ever constrain it, never loosen or escape it.
func TestMergeRouting(t *testing.T) {
	b := func(v bool) *bool { return &v }
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name          string
		base, overlay *v1.OpenRouterRouting
		want          *v1.OpenRouterRouting
	}{
		// --- the core narrowing rules, one row per field ---
		{
			name:    "Only intersects (agent cannot widen)",
			base:    &v1.OpenRouterRouting{Only: []string{"anthropic", "openai"}},
			overlay: &v1.OpenRouterRouting{Only: []string{"anthropic", "google"}},
			want:    &v1.OpenRouterRouting{Only: []string{"anthropic"}},
		},
		{
			name:    "Ignore unions",
			base:    &v1.OpenRouterRouting{Ignore: []string{"x"}},
			overlay: &v1.OpenRouterRouting{Ignore: []string{"y"}},
			want:    &v1.OpenRouterRouting{Ignore: []string{"x", "y"}},
		},
		{
			name:    "MaxPrice tightens to min",
			base:    &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 5, Completion: 5}},
			overlay: &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 2, Completion: 9}},
			want:    &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 2, Completion: 5}},
		},
		{
			name:    "Sort/CostQuality: agent overrides",
			base:    &v1.OpenRouterRouting{Sort: "throughput"},
			overlay: &v1.OpenRouterRouting{Sort: "price", CostQualityTradeoff: f(0.2)},
			want:    &v1.OpenRouterRouting{Sort: "price", CostQualityTradeoff: f(0.2)},
		},
		{
			name:    "CostQualityTradeoff: overlay nil ⇒ base's kept",
			base:    &v1.OpenRouterRouting{CostQualityTradeoff: f(0.5)},
			overlay: &v1.OpenRouterRouting{},
			want:    &v1.OpenRouterRouting{CostQualityTradeoff: f(0.5)},
		},
		{
			name:    "AllowFallbacks logical AND",
			base:    &v1.OpenRouterRouting{AllowFallbacks: b(true)},
			overlay: &v1.OpenRouterRouting{AllowFallbacks: b(false)},
			want:    &v1.OpenRouterRouting{AllowFallbacks: b(false)},
		},
		{
			name:    "AllowFallbacks: admin false cannot be flipped on (pins AND vs overlay-wins)",
			base:    &v1.OpenRouterRouting{AllowFallbacks: b(false)},
			overlay: &v1.OpenRouterRouting{AllowFallbacks: b(true)},
			want:    &v1.OpenRouterRouting{AllowFallbacks: b(false)},
		},
		{name: "nil overlay returns base", base: &v1.OpenRouterRouting{Sort: "price"}, overlay: nil, want: &v1.OpenRouterRouting{Sort: "price"}},
		{name: "nil base returns overlay", base: nil, overlay: &v1.OpenRouterRouting{Sort: "price"}, want: &v1.OpenRouterRouting{Sort: "price"}},

		// --- intersect edge cases ---
		{
			name:    "both nil ⇒ nil",
			base:    nil,
			overlay: nil,
			want:    nil,
		},
		{
			name:    "Only: empty intersection falls back to BASE (fail-closed, never widen)",
			base:    &v1.OpenRouterRouting{Only: []string{"anthropic", "openai"}},
			overlay: &v1.OpenRouterRouting{Only: []string{"google"}},
			want:    &v1.OpenRouterRouting{Only: []string{"anthropic", "openai"}},
		},
		{
			name:    "Models: empty intersection falls back to BASE",
			base:    &v1.OpenRouterRouting{Models: []string{"a/b", "c/d"}},
			overlay: &v1.OpenRouterRouting{Models: []string{"e/f"}},
			want:    &v1.OpenRouterRouting{Models: []string{"a/b", "c/d"}},
		},
		{
			name:    "Models: base empty ⇒ overlay's items adopted",
			base:    &v1.OpenRouterRouting{},
			overlay: &v1.OpenRouterRouting{Models: []string{"x/y"}},
			want:    &v1.OpenRouterRouting{Models: []string{"x/y"}},
		},
		{
			name:    "Models: overlay empty ⇒ base's items kept",
			base:    &v1.OpenRouterRouting{Models: []string{"a/b", "c/d"}},
			overlay: &v1.OpenRouterRouting{},
			want:    &v1.OpenRouterRouting{Models: []string{"a/b", "c/d"}},
		},
		{
			name:    "AllowedModels intersects preserving overlay order (agent preference within admin set)",
			base:    &v1.OpenRouterRouting{AllowedModels: []string{"a", "b", "c"}},
			overlay: &v1.OpenRouterRouting{AllowedModels: []string{"c", "a"}},
			want:    &v1.OpenRouterRouting{AllowedModels: []string{"c", "a"}},
		},
		{
			name:    "AllowedModels: empty intersection falls back to BASE",
			base:    &v1.OpenRouterRouting{AllowedModels: []string{"a", "b"}},
			overlay: &v1.OpenRouterRouting{AllowedModels: []string{"z"}},
			want:    &v1.OpenRouterRouting{AllowedModels: []string{"a", "b"}},
		},

		// --- Order: overlay-wins (ranks, doesn't allow/deny) not intersect ---
		{
			name:    "Order: overlay replaces base outright (ranking, not an allow-list)",
			base:    &v1.OpenRouterRouting{Order: []string{"p1", "p2"}},
			overlay: &v1.OpenRouterRouting{Order: []string{"p3"}},
			want:    &v1.OpenRouterRouting{Order: []string{"p3"}},
		},
		{
			name:    "Order: overlay unset ⇒ base's kept",
			base:    &v1.OpenRouterRouting{Order: []string{"p1", "p2"}},
			overlay: &v1.OpenRouterRouting{},
			want:    &v1.OpenRouterRouting{Order: []string{"p1", "p2"}},
		},

		// --- MaxPrice: one-side-nil and per-field 0=unset ---
		{
			name:    "MaxPrice: base nil ⇒ overlay's copied",
			base:    &v1.OpenRouterRouting{},
			overlay: &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 2, Completion: 3}},
			want:    &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 2, Completion: 3}},
		},
		{
			name:    "MaxPrice: overlay nil ⇒ base's copied",
			base:    &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 2, Completion: 3}},
			overlay: &v1.OpenRouterRouting{},
			want:    &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 2, Completion: 3}},
		},
		{
			name:    "MaxPrice: 0 on one side per-field means unset, not tightest — take other side's",
			base:    &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 0, Completion: 5}},
			overlay: &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 3, Completion: 0}},
			want:    &v1.OpenRouterRouting{MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 3, Completion: 5}},
		},

		// --- DataCollection: deny-sticky (an agent may tighten privacy, never loosen it) ---
		{
			name:    "DataCollection: agent 'allow' cannot loosen admin 'deny' (pins the privacy escape)",
			base:    &v1.OpenRouterRouting{DataCollection: "deny"},
			overlay: &v1.OpenRouterRouting{DataCollection: "allow"},
			want:    &v1.OpenRouterRouting{DataCollection: "deny"},
		},
		{
			name:    "DataCollection: agent 'deny' tightens admin 'allow'",
			base:    &v1.OpenRouterRouting{DataCollection: "allow"},
			overlay: &v1.OpenRouterRouting{DataCollection: "deny"},
			want:    &v1.OpenRouterRouting{DataCollection: "deny"},
		},
		{
			name:    "DataCollection: base unset ⇒ overlay's 'allow' adopted",
			base:    &v1.OpenRouterRouting{},
			overlay: &v1.OpenRouterRouting{DataCollection: "allow"},
			want:    &v1.OpenRouterRouting{DataCollection: "allow"},
		},
		{
			name:    "DataCollection: overlay unset ⇒ base's 'allow' kept",
			base:    &v1.OpenRouterRouting{DataCollection: "allow"},
			overlay: &v1.OpenRouterRouting{},
			want:    &v1.OpenRouterRouting{DataCollection: "allow"},
		},

		// --- RequireParameters: logical OR (true is the restrictive pole — the
		// agent may add the requirement, never drop one admin set) ---
		{
			name:    "RequireParameters: agent cannot drop admin's requirement (pins OR vs AND and overlay-wins)",
			base:    &v1.OpenRouterRouting{RequireParameters: b(true)},
			overlay: &v1.OpenRouterRouting{RequireParameters: b(false)},
			want:    &v1.OpenRouterRouting{RequireParameters: b(true)},
		},
		{
			name:    "RequireParameters: agent may add the requirement",
			base:    &v1.OpenRouterRouting{RequireParameters: b(false)},
			overlay: &v1.OpenRouterRouting{RequireParameters: b(true)},
			want:    &v1.OpenRouterRouting{RequireParameters: b(true)},
		},
		{
			name:    "RequireParameters: nil overlay side ⇒ base's kept",
			base:    &v1.OpenRouterRouting{RequireParameters: b(true)},
			overlay: &v1.OpenRouterRouting{},
			want:    &v1.OpenRouterRouting{RequireParameters: b(true)},
		},
		{
			name:    "AllowFallbacks: nil base side ⇒ overlay's kept",
			base:    &v1.OpenRouterRouting{},
			overlay: &v1.OpenRouterRouting{AllowFallbacks: b(false)},
			want:    &v1.OpenRouterRouting{AllowFallbacks: b(false)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mergeRouting(tc.base, tc.overlay))
		})
	}
}

// TestMergeRouting_NonAliasing proves mergeRouting never mutates or aliases
// either input: mutating base/overlay (and their nested pointers/slices) after
// the call must leave the merged result unaffected. An aliased slice or pointer
// would let a later, unrelated mutation of the controller-owned catalog object
// silently corrupt an already-resolved EffectiveSettings.
func TestMergeRouting_NonAliasing(t *testing.T) {
	base := &v1.OpenRouterRouting{
		Models:         []string{"a/b", "c/d"},
		Only:           []string{"anthropic", "openai"},
		Ignore:         []string{"x"},
		Order:          []string{"p1"},
		AllowFallbacks: func() *bool { v := true; return &v }(),
		MaxPrice:       &v1.OpenRouterMaxPrice{Prompt: 5, Completion: 5},
	}
	overlay := &v1.OpenRouterRouting{
		Models:   []string{"a/b"},
		Only:     []string{"anthropic"},
		Ignore:   []string{"y"},
		MaxPrice: &v1.OpenRouterMaxPrice{Prompt: 2, Completion: 9},
	}

	got := mergeRouting(base, overlay)
	want := &v1.OpenRouterRouting{
		Models:         []string{"a/b"},
		Only:           []string{"anthropic"},
		Ignore:         []string{"x", "y"},
		Order:          []string{"p1"},
		AllowFallbacks: func() *bool { v := true; return &v }(),
		MaxPrice:       &v1.OpenRouterMaxPrice{Prompt: 2, Completion: 5},
	}
	assert.Equal(t, want, got, "sanity: merge result correct before mutation")

	// Mutate every input slice/pointer after the call.
	base.Models[0] = "MUTATED"
	base.Only[0] = "MUTATED"
	base.Ignore[0] = "MUTATED"
	base.Order[0] = "MUTATED"
	*base.AllowFallbacks = false
	base.MaxPrice.Prompt = 999
	overlay.Models[0] = "MUTATED"
	overlay.Only[0] = "MUTATED"
	overlay.Ignore[0] = "MUTATED"
	overlay.MaxPrice.Completion = 999

	assert.Equal(t, want, got, "result must be unaffected by post-merge mutation of either input")
	assert.NotSame(t, base.MaxPrice, got.MaxPrice, "MaxPrice result must be a fresh pointer, not base's")
	assert.NotSame(t, overlay.MaxPrice, got.MaxPrice, "MaxPrice result must be a fresh pointer, not overlay's")
	assert.NotSame(t, base.AllowFallbacks, got.AllowFallbacks, "AllowFallbacks result must be a fresh pointer")
}
