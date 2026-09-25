package skillbundle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTarGzDeterministicDigest(t *testing.T) {
	files := map[string][]byte{
		"SKILL.md":     []byte("---\nname: x\n---\nbody"),
		"scripts/a.sh": []byte("echo hi"),
	}
	gz1, d1, err := TarGz(files)
	require.NoError(t, err)
	// Reordered map iteration must NOT change the bytes/digest (deterministic).
	gz2, d2, err := TarGz(map[string][]byte{
		"scripts/a.sh": []byte("echo hi"),
		"SKILL.md":     []byte("---\nname: x\n---\nbody"),
	})
	require.NoError(t, err)
	assert.Equal(t, d1, d2, "digest must be order-independent")
	assert.Equal(t, gz1, gz2, "archive bytes must be order-independent")
	assert.NotEmpty(t, d1)

	// Different content → different digest.
	_, d3, err := TarGz(map[string][]byte{"SKILL.md": []byte("other")})
	require.NoError(t, err)
	assert.NotEqual(t, d1, d3)
}
