package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// urlSafeAlphabet covers the base64url alphabet (no padding).
var urlSafeAlphabet = regexp.MustCompile(`^[A-Za-z0-9\-_]+$`)

func TestNewPKCE_Lengths(t *testing.T) {
	p, err := NewPKCE()
	require.NoError(t, err, "NewPKCE")

	// Verifier: 32 bytes → base64url-no-padding = ceil(32*4/3) = 43 chars.
	assert.GreaterOrEqual(t, len(p.Verifier), 43, "verifier length")
	assert.LessOrEqual(t, len(p.Verifier), 128, "verifier length")
	// Challenge: sha256(verifier) = 32 bytes → base64url-no-padding = 43 chars.
	assert.Equal(t, 43, len(p.Challenge), "challenge length")
	// State: 16 bytes → base64url-no-padding = 22 chars.
	assert.Equal(t, 22, len(p.State), "state length")
}

func TestNewPKCE_Charset(t *testing.T) {
	p, err := NewPKCE()
	require.NoError(t, err, "NewPKCE")
	for name, s := range map[string]string{
		"Verifier":  p.Verifier,
		"Challenge": p.Challenge,
		"State":     p.State,
	} {
		assert.Regexp(t, urlSafeAlphabet, s, "%s contains non-URL-safe chars", name)
	}
}

func TestNewPKCE_ChallengeMatchesSHA256(t *testing.T) {
	p, err := NewPKCE()
	require.NoError(t, err, "NewPKCE")
	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	assert.Equal(t, want, p.Challenge, "challenge should be sha256(verifier)")
}

func TestNewPKCE_Uniqueness(t *testing.T) {
	p1, err := NewPKCE()
	require.NoError(t, err, "NewPKCE first call")
	p2, err := NewPKCE()
	require.NoError(t, err, "NewPKCE second call")
	assert.NotEqual(t, p1.Verifier, p2.Verifier, "two consecutive verifiers identical (crypto/rand broken?)")
	assert.NotEqual(t, p1.State, p2.State, "two consecutive states identical (crypto/rand broken?)")
}
