package categories

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// TestSlotBindRefusedIsARegisteredNotice pins the row this feature adds: a
// mint-time pin refusal has a notice category to publish, it is a NOTICE (no
// decision leg, so it touches no approval routing or autoApprove list), and its
// degraded tone obliges a next step — a reader told a target was dropped must
// be told what they can do instead.
func TestSlotBindRefusedIsARegisteredNotice(t *testing.T) {
	c, ok := channelinteractions.Get(SlotBindRefused)
	require.True(t, ok, "SlotBindRefused is not registered")
	assert.True(t, c.Notice, "SlotBindRefused must be a notice, not a prompt")
	assert.Equal(t, channelinteractions.ToneDegraded, c.Tone,
		"a dropped target is degraded: the reader who assumed it took effect is wrong")
	assert.True(t, c.Tone.RequiresNextStep(),
		"the reader must be told what to do about the target that was not added")
}
