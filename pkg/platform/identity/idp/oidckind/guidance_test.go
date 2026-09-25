package oidckind

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
// The redirect URI is measured separately, by tui.Address: it is delivered by
// the Question rather than composed into this block, so that ONE rule decides
// what happens to an address a note cannot hold. What is left here is prose,
// and prose that wraps is untidy rather than broken — but it is still the only
// thing telling the user what to go and do, so it is held to the budget too.
func TestGuidanceFitsTheNote(t *testing.T) {
	assert.Empty(t, tui.RailedNoteBudget().Overflows(issuerIntro),
		"these guidance lines are wider than a note can render")
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
