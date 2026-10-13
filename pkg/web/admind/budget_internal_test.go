package admind

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestToolCostUSD_NilByToolContributesZero(t *testing.T) {
	assert.Equal(t, 0.0, float64(toolCostUSD(nil)), "nil/empty -> 0")
	assert.InDelta(t, 19.02, float64(toolCostUSD([]spiceboxv1alpha1.ToolCostBucket{
		{Tool: "claude-oauth", AmountMicroUSD: 19_020_000, PricingKnown: true},
	})), 1e-9)
}

func TestToolCostUSD_ExcludesSubscriptionEquivalent(t *testing.T) {
	// A subscription-equivalent bucket is a flat-rate estimate, excluded from
	// spend; only the metered bucket counts.
	got := toolCostUSD([]spiceboxv1alpha1.ToolCostBucket{
		{Tool: "aider", AmountMicroUSD: 2_000_000, PricingKnown: true},
		{Tool: "claude-oauth", AmountMicroUSD: 5_000_000, PricingKnown: true, SubscriptionEquivalent: true},
	})
	assert.InDelta(t, 2.00, float64(got), 1e-9, "only the $2 metered tool counts; the $5 subscription tool is excluded")
}
