package channelinteractions

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// AllTones is the single source every consumer that must handle EVERY tone
// reads from. A tone added to the type but missing here silently stops being
// covered by the chip guards downstream, which is exactly how an unmapped tone
// reaches a renderer's default and paints one meaning as another.
func TestAllTonesCoversEveryValidTone(t *testing.T) {
	for _, tone := range AllTones() {
		assert.True(t, tone.Valid(), "AllTones returned %q, which Valid() rejects", tone)
	}
	assert.Contains(t, AllTones(), ToneUnavailable)
	assert.Len(t, AllTones(), 8, "a new tone must be added to AllTones")
}

func TestToneUnavailableRequiresNextStep(t *testing.T) {
	assert.True(t, ToneUnavailable.RequiresNextStep(),
		"a reader told something is off, and not told who can turn it on, is stuck")
}

// ToneUnavailable exists precisely because ToneDegraded's contract ("the user
// can usually get what they wanted by trying again") is wrong for it.
func TestToneUnavailableIsNotDegraded(t *testing.T) {
	assert.NotEqual(t, ToneDegraded, ToneUnavailable)
	assert.True(t, ToneDegraded.Valid())
}

func TestAllGlyphsCoversEveryValidGlyph(t *testing.T) {
	for _, g := range AllGlyphs() {
		assert.True(t, g.Valid(), "AllGlyphs returned %q, which Valid() rejects", g)
	}
	assert.Contains(t, AllGlyphs(), GlyphWarning)
	assert.NotContains(t, AllGlyphs(), GlyphDefault,
		"the zero value means 'draw the tone chip' and has no chip of its own")
}
