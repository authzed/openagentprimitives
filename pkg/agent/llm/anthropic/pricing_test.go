package anthropic

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPricing_KnownAndUnknown(t *testing.T) {
	var p Provider
	got, ok := p.Pricing("claude-opus-4-8")
	assert.True(t, ok, "opus 4.8 must be priced")
	assert.Equal(t, 5.0, got.InputPerMTok)
	assert.Equal(t, 25.0, got.OutputPerMTok)
	assert.Equal(t, 0.5, got.CacheReadPerMTok, "cache read = 0.1x input")
	assert.Equal(t, "USD", got.Currency)

	_, ok = p.Pricing("some-unknown-model")
	assert.False(t, ok, "unknown model is unpriced (no fabricated number)")
}
