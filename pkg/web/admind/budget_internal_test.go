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
