package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMintSetFingerprint_StableAndTracksTheMintSet(t *testing.T) {
	base := MintSetFingerprint()
	assert.Equal(t, base, MintSetFingerprint(), "a pure function of compiled-in values")

	orig := MemoryTokenSecretNames
	t.Cleanup(func() { MemoryTokenSecretNames = orig })
	MemoryTokenSecretNames = append(append([]string{}, orig...), "spicebox-newcomponent-memory-token")
	assert.NotEqual(t, base, MintSetFingerprint(), "adding a Secret to the mint set changes the fingerprint")
}
