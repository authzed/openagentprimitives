package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestRoutingToLLM(t *testing.T) {
	boolp := func(b bool) *bool { return &b }
	floatp := func(f float64) *float64 { return &f }

	cases := []struct {
		name string
		in   *spiceboxv1alpha1.OpenRouterRouting
		want *llm.OpenRouterRouting
	}{
		{
			name: "nil in ⇒ nil out",
			in:   nil,
			want: nil,
		},
		{
			name: "empty struct in ⇒ empty struct out (no nil-pointer traps)",
			in:   &spiceboxv1alpha1.OpenRouterRouting{},
			want: &llm.OpenRouterRouting{},
		},
		{
			name: "fully populated incl. auto-router hints + MaxPrice",
			in: &spiceboxv1alpha1.OpenRouterRouting{
				Models:              []string{"a/b", "c/d"},
				Sort:                "price",
				Order:               []string{"provider-a", "provider-b"},
				Only:                []string{"provider-a"},
				Ignore:              []string{"provider-c"},
				AllowFallbacks:      boolp(true),
				RequireParameters:   boolp(false),
				DataCollection:      "deny",
				MaxPrice:            &spiceboxv1alpha1.OpenRouterMaxPrice{Prompt: 0.001, Completion: 0.002},
				AllowedModels:       []string{"a/b"},
				CostQualityTradeoff: floatp(3),
			},
			want: &llm.OpenRouterRouting{
				Models:              []string{"a/b", "c/d"},
				Sort:                "price",
				Order:               []string{"provider-a", "provider-b"},
				Only:                []string{"provider-a"},
				Ignore:              []string{"provider-c"},
				AllowFallbacks:      boolp(true),
				RequireParameters:   boolp(false),
				DataCollection:      "deny",
				MaxPrice:            &llm.OpenRouterMaxPrice{Prompt: 0.001, Completion: 0.002},
				AllowedModels:       []string{"a/b"},
				CostQualityTradeoff: floatp(3),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routingToLLM(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("non-aliasing: mutating the input after conversion does not change the output", func(t *testing.T) {
		in := &spiceboxv1alpha1.OpenRouterRouting{
			Models:         []string{"a/b"},
			AllowFallbacks: boolp(true),
		}
		got := routingToLLM(in)
		require.NotNil(t, got)

		// Mutate the source slice element and pointer target in place.
		in.Models[0] = "mutated"
		*in.AllowFallbacks = false

		assert.Equal(t, "a/b", got.Models[0], "slice must be a fresh allocation, not aliased")
		assert.True(t, *got.AllowFallbacks, "pointer target must be a fresh allocation, not aliased")
	})
}
