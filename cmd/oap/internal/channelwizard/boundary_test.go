package channelwizard

import (
	"go/build"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPackageNeverImportsACommandThatDrivesIt: this package exists so that two
// commands can share one driver, which only works while it stays a LEAF under
// both of them. An import back into either is the boundary being in the wrong
// place — the driver reaching for something that belongs to a caller — and it
// is worth catching as a named failure rather than as whatever the compiler
// says next.
//
// THE COMPILER ALREADY REFUSES BOTH TODAY, and this test does not pretend
// otherwise: channelcmd imports this package directly, and agentcmd reaches it
// through identitycmd, so either import is an import cycle right now. What the
// compiler does NOT do is keep refusing them. Both cycles exist only because
// of an edge somewhere else — drop identitycmd's use of
// CheckInteractionRequired and the agentcmd direction quietly becomes legal
// again, with nothing announcing that the boundary just opened. This states
// the rule where the rule lives, so it survives its incidental enforcement
// going away.
//
// Read off the source rather than asserted structurally, because a structural
// assertion (a var of a type from the other package) would itself BE the
// import it is trying to forbid.
func TestPackageNeverImportsACommandThatDrivesIt(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	require.NoError(t, err, "read this package's own import lists")

	all := slices.Concat(pkg.Imports, pkg.TestImports, pkg.XTestImports)
	// Without this the whole test passes vacuously on an empty list — which is
	// exactly what a mis-resolved directory would produce.
	require.Contains(t, all, "github.com/authzed/openagentprimitives/pkg/channels/channelkinds",
		"the import list must actually have been read; the driver dispatches through channelkinds")

	for _, forbidden := range []string{
		"github.com/authzed/openagentprimitives/cmd/oap/internal/channelcmd",
		"github.com/authzed/openagentprimitives/cmd/oap/internal/agentcmd",
	} {
		assert.NotContains(t, all, forbidden,
			"the driver must not import a command that drives it: %s", forbidden)
	}
}
