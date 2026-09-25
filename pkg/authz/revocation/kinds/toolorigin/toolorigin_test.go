package toolorigin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvalidateThenIsRevoked(t *testing.T) {
	s := New()
	assert.Equal(t, "tool-origin", s.Kind())
	assert.False(t, s.IsRevoked("mcpserver/x"))
	require.NoError(t, s.Invalidate("mcpserver/x"))
	assert.True(t, s.IsRevoked("mcpserver/x"))
	assert.False(t, s.IsRevoked("toolkit/git"))
	require.NoError(t, s.Invalidate("mcpserver/x")) // idempotent
	assert.True(t, s.IsRevoked("mcpserver/x"), "double Invalidate stays revoked (idempotent)")
}

// The noun is substituted into chat copy a surface renders as trusted markup,
// so it has to be a phrase a person recognises — not the wire token Kind()
// returns, and not the mcpserver/<name> origin string, which is operator
// vocabulary and belongs nowhere near a chat thread.
func TestToolOriginNamesWhatItWithdraws(t *testing.T) {
	noun := New().Noun()
	assert.Equal(t, "a set of tools", noun)
	for _, internal := range []string{"origin", "mcpserver", "tool-origin"} {
		assert.NotContains(t, noun, internal,
			"internal vocabulary %q must not ride into user copy on the noun", internal)
	}
}
