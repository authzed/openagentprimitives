package categories

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// One category cannot carry three meanings: Tone is a fixed property of the
// row by design, so the single AttachmentReadFailed category painted a
// switched-off capability the same orange as a transient fetch failure, and
// told the reader to try again.
func TestAttachmentNoticeCategoriesAreDistinctlyToned(t *testing.T) {
	cases := []struct {
		name  string
		cat   string
		tone  channelinteractions.Tone
		glyph channelinteractions.Glyph
	}{
		{
			name: "not enabled: unavailable, no glyph",
			cat:  AttachmentNotEnabled,
			tone: channelinteractions.ToneUnavailable,
		},
		{
			name:  "unreadable: unavailable, warning glyph",
			cat:   AttachmentUnreadable,
			tone:  channelinteractions.ToneUnavailable,
			glyph: channelinteractions.GlyphWarning,
		},
		{
			name: "transient: degraded, no glyph",
			cat:  AttachmentReadFailed,
			tone: channelinteractions.ToneDegraded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := channelinteractions.Get(tc.cat)
			require.True(t, ok, "category %q is not registered", tc.cat)
			assert.Equal(t, tc.tone, c.Tone)
			assert.Equal(t, tc.glyph, c.Glyph)
			assert.True(t, c.Notice, "all three are notices, not prompts")
		})
	}
}

// The tone that made the reported card wrong. Guarded explicitly so a future
// edit cannot quietly fold "switched off" back onto "try again".
func TestNotEnabledIsNeverDegraded(t *testing.T) {
	c, ok := channelinteractions.Get(AttachmentNotEnabled)
	require.True(t, ok)
	assert.NotEqual(t, channelinteractions.ToneDegraded, c.Tone,
		"nothing was attempted; ToneDegraded promises retrying helps, and it cannot")
	assert.True(t, c.Tone.RequiresNextStep(),
		"the reader must be told who can turn it on")
}
