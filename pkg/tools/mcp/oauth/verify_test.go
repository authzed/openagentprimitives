package oauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyS256MatchesNewPKCE(t *testing.T) {
	p, err := NewPKCE()
	require.NoError(t, err)
	assert.True(t, VerifyS256(p.Verifier, p.Challenge))
	assert.False(t, VerifyS256(p.Verifier+"x", p.Challenge))
	assert.False(t, VerifyS256("", p.Challenge))
	assert.False(t, VerifyS256(p.Verifier, ""))
}
