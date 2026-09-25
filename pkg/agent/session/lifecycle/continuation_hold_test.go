package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContinuationDisposition_Held_refuses(t *testing.T) {
	got := ContinuationDisposition(State{Phase: PhaseHeld})
	refuse, ok := got.(DispRefuse)
	require.True(t, ok, "a held session must refuse inbound messages, not resume them; got %T", got)
	assert.Equal(t, "ForensicHold", refuse.Reason)
}

func TestIsPolicyHalt_ForensicHold(t *testing.T) {
	assert.True(t, IsPolicyHalt("ForensicHold"),
		"a held session's transcript must not be inherited by a different-user takeover")
}
