package googlekind

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/idpscreens"
)

// shortCallback is a redirect URI a note can carry whole; longCallback is one
// it cannot. Both are the shape a real cluster produces — the second is simply
// hosted at a longer name.
const (
	shortCallback = "https://ap.demo-cluster.example/oidc/callback/idp"
	longCallback  = "https://agents.platform.internal.demo-corporation.example/oidc/callback/idp"
)

// TestGuidanceFitsTheNote measures every line of this kind's own guidance
// against the column budget a note actually gets.
//
// This block is the longest of the three wizards' — it walks the user through
// the Google console — so it is the one most likely to grow past the budget on
// the next edit. The redirect URI is measured separately, by tui.Address, since
// the Question delivers it rather than this block composing it.
func TestGuidanceFitsTheNote(t *testing.T) {
	assert.Empty(t, tui.RailedNoteBudget().Overflows(consoleIntro),
		"these guidance lines are wider than a note can render")
	assert.Contains(t, consoleIntro, clientIDSuffix,
		"the guidance must say what a Google client ID looks like")
}

// TestTheRedirectURIIsDeliveredByTheBudgetsRule pins the two halves of the
// address rule this kind depends on: an address a note can carry is carried
// whole, and one it cannot is CUT into the note and marked — so the question
// still names what the guidance calls "the address below" — while the CLI
// prints the whole one outside the form, because this address is the one thing
// the user has to copy and a summary line after the run is too late.
func TestTheRedirectURIIsDeliveredByTheBudgetsRule(t *testing.T) {
	budget := tui.RailedNoteBudget()

	assert.True(t, idpscreens.CallbackAddress(shortCallback).Fits(budget),
		"an ordinary cluster address must reach the user inside the question")

	require.False(t, idpscreens.CallbackAddress(longCallback).Fits(budget),
		"this fixture must be too wide for a note, or the test proves nothing")
}
