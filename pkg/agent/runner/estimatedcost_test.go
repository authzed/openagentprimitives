package runner

import (
	"testing"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func TestEstimatedSessionCostType(t *testing.T) {
	c := spiceboxv1alpha1.EstimatedSessionCost{
		AmountMicroUSD: 420000, Currency: "USD", Model: "claude-opus-4-8", PricingKnown: true,
	}
	assert.Equal(t, int64(420000), c.AmountMicroUSD)
	assert.True(t, c.PricingKnown)
}
