package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOpenRouterRouting_ZeroValueIsInert(t *testing.T) {
	var r *OpenRouterRouting
	assert.Nil(t, r, "nil routing means no routing preferences")
}

func TestResponseCarriesServedModelAndCost(t *testing.T) {
	cost := 0.0123
	resp := Response{Model: "openrouter/anthropic/claude-3.5-sonnet", Usage: Usage{CostUSD: &cost}}
	assert.Equal(t, "openrouter/anthropic/claude-3.5-sonnet", resp.Model)
	if assert.NotNil(t, resp.Usage.CostUSD) {
		assert.InDelta(t, 0.0123, *resp.Usage.CostUSD, 1e-9)
	}
}
