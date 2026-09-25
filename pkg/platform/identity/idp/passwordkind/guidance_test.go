package passwordkind

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/stretchr/testify/assert"
)

// TestGuidanceFitsTheNote measures every line the first screen renders against
// the column budget a note actually gets.
//
// There is no redirect URI in this wizard — nothing federates — so what wraps
// here is prose rather than an address. Still asserted, because a note that
// wraps mid-sentence is what tells a reader the box is broken rather than the
// text, and this block is the only thing explaining what the password is FOR.
//
// Measured with tui.RailedNoteBudget() rather than a local copy of
// the arithmetic; see the same test in oidckind for what that budget is and
// what it is optimistic about.
func TestGuidanceFitsTheNote(t *testing.T) {
	assert.Empty(t, tui.RailedNoteBudget().Overflows(passwordIntro),
		"these guidance lines are wider than a note can render")
}

// TestGuidanceNamesWhereTheseCredentialsAreUsed keeps the block from decaying
// into a bare "enter a password": this is the only place the run says what the
// password unlocks, which is what tells the user whether to set one at all.
func TestGuidanceNamesWhereTheseCredentialsAreUsed(t *testing.T) {
	assert.Contains(t, passwordIntro, "/admin")
}
