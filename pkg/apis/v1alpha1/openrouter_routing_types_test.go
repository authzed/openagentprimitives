package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenRouterRoutingDeepCopy populates every field of OpenRouterRouting
// (including pointers and slices), deep-copies it, mutates the original, and
// asserts the copy is unaffected — proving DeepCopy doesn't alias any
// pointer/slice field (i.e. that controller-gen regen actually ran and
// produced a real deep copy, not a shallow struct assignment).
func TestOpenRouterRoutingDeepCopy(t *testing.T) {
	allowFallbacks := true
	requireParameters := true
	tradeoff := 3.0

	orig := OpenRouterRouting{
		Models:            []string{"model-a", "model-b"},
		Sort:              "price",
		Order:             []string{"provider-a", "provider-b"},
		Only:              []string{"provider-a"},
		Ignore:            []string{"provider-c"},
		AllowFallbacks:    &allowFallbacks,
		RequireParameters: &requireParameters,
		DataCollection:    "deny",
		MaxPrice: &OpenRouterMaxPrice{
			Prompt:     0.001,
			Completion: 0.002,
		},
		AllowedModels:       []string{"model-a"},
		CostQualityTradeoff: &tradeoff,
	}

	cp := orig.DeepCopy()
	require.NotNil(t, cp)

	// Sanity: the copy starts out equal.
	assert.Equal(t, orig, *cp)

	// Mutate every pointer/slice field on the original; the copy must be
	// unaffected if DeepCopy actually deep-copied rather than aliased.
	orig.Models[0] = "mutated"
	orig.Order[0] = "mutated"
	orig.Only[0] = "mutated"
	orig.Ignore[0] = "mutated"
	*orig.AllowFallbacks = false
	*orig.RequireParameters = false
	orig.MaxPrice.Prompt = 999
	orig.AllowedModels[0] = "mutated"
	*orig.CostQualityTradeoff = 999

	assert.Equal(t, "model-a", cp.Models[0], "DeepCopy must not alias Models")
	assert.Equal(t, "provider-a", cp.Order[0], "DeepCopy must not alias Order")
	assert.Equal(t, "provider-a", cp.Only[0], "DeepCopy must not alias Only")
	assert.Equal(t, "provider-c", cp.Ignore[0], "DeepCopy must not alias Ignore")
	assert.True(t, *cp.AllowFallbacks, "DeepCopy must not alias AllowFallbacks")
	assert.True(t, *cp.RequireParameters, "DeepCopy must not alias RequireParameters")
	assert.Equal(t, 0.001, cp.MaxPrice.Prompt, "DeepCopy must not alias MaxPrice")
	assert.Equal(t, "model-a", cp.AllowedModels[0], "DeepCopy must not alias AllowedModels")
	assert.Equal(t, 3.0, *cp.CostQualityTradeoff, "DeepCopy must not alias CostQualityTradeoff")
}

// TestOpenRouterRoutingDeepCopyNilFields exercises the nil-pointer / nil-slice
// path: DeepCopy of a zero-value struct (and of a nil *OpenRouterRouting via
// ModelCatalogEntry) must not panic and must produce an equivalent zero value.
func TestOpenRouterRoutingDeepCopyNilFields(t *testing.T) {
	var orig OpenRouterRouting
	cp := orig.DeepCopy()
	require.NotNil(t, cp)
	assert.Equal(t, orig, *cp)

	entry := ModelCatalogEntry{Name: "openrouter/auto", Provider: "openrouter"}
	require.Nil(t, entry.Routing)
	cpEntry := entry.DeepCopy()
	assert.Nil(t, cpEntry.Routing)

	entry.Routing = &OpenRouterRouting{Sort: "latency"}
	cpEntry = entry.DeepCopy()
	require.NotNil(t, cpEntry.Routing)
	assert.Equal(t, "latency", cpEntry.Routing.Sort)

	entry.Routing.Sort = "mutated"
	assert.Equal(t, "latency", cpEntry.Routing.Sort, "DeepCopy must not alias the Routing pointer")
}
