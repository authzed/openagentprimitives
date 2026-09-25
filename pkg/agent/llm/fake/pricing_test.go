package fake

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/stretchr/testify/assert"
)

func TestFakePricing(t *testing.T) {
	p := New(nil)
	_, ok := p.Pricing("m")
	assert.False(t, ok, "unset → unpriced")
	p.SetPricing(map[string]llm.ModelPricing{"m": {InputPerMTok: 1, Currency: "USD"}})
	got, ok := p.Pricing("m")
	assert.True(t, ok)
	assert.Equal(t, 1.0, got.InputPerMTok)
}
