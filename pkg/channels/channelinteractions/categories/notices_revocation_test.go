package categories

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// A withdrawal of access that did not take effect has its own row, and the tone
// assertions below are the whole point of it: a ToneDegraded row promises the
// reader that retrying is the remedy, and re-sending a message withdraws
// nothing.
func TestRevocationNotAppliedRegistered(t *testing.T) {
	c, ok := channelinteractions.Get(RevocationNotApplied)
	require.True(t, ok, "importing this package registers revocation_not_applied")

	assert.True(t, c.Notice, "there is no decision to make; the withdrawal already happened elsewhere")
	assert.Equal(t, channelinteractions.TonePrivacy, c.Tone,
		"this is about who can still reach whose data, not about a failure a retry clears")
	assert.False(t, c.Terminal,
		"the session continues and the reader can still act — end it, or withdraw upstream")
	assert.Equal(t, channelinteractions.ResurfaceNone, c.Resurface)
}

// TonePrivacy requires a NextStep, which is the property that makes this row the
// right one: a reader told that access they revoked may still be live and given
// nothing to do about it is worse off than one who was told nothing.
func TestRevocationNotAppliedDemandsANextStep(t *testing.T) {
	c, ok := channelinteractions.Get(RevocationNotApplied)
	require.True(t, ok)
	assert.True(t, c.Tone.RequiresNextStep(),
		"a publisher must not be able to post this row without saying what to do about it")
}
