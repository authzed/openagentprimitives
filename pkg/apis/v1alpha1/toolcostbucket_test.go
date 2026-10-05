package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEstimatedSessionCost_DeepCopy_ByTool(t *testing.T) {
	in := &EstimatedSessionCost{
		AmountMicroUSD: 19_860_000,
		PricingKnown:   true,
		ByModel:        []ModelCostBucket{{Model: "anthropic/claude-sonnet-5", AmountMicroUSD: 840_000, PricingKnown: true}},
		ByTool:         []ToolCostBucket{{Tool: "claude-oauth", AmountMicroUSD: 19_020_000, PricingKnown: true}},
	}
	out := in.DeepCopy()
	require.Len(t, out.ByTool, 1)
	assert.Equal(t, "claude-oauth", out.ByTool[0].Tool)
	assert.Equal(t, int64(19_020_000), out.ByTool[0].AmountMicroUSD)
	// Deep, not shallow: mutating the copy must not touch the original.
	out.ByTool[0].AmountMicroUSD = 0
	assert.Equal(t, int64(19_020_000), in.ByTool[0].AmountMicroUSD)
}
