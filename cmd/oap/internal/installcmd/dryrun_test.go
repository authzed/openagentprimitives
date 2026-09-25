package installcmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// TestDryRunServerIsRefusedByBothCommands: `oap install` and `oap clean` must
// answer an unimplemented mode the same way. A user who learns --dry-run on one
// carries it to the other, and a mode that one accepts (silently doing a
// client-side print) while the other refuses is a difference nothing announces.
//
// Both refusals must land before any cluster access — these run with no
// kubeconfig and no fake bundle, so reaching a cluster would fail differently
// (or hang), not return this error.
func TestDryRunServerIsRefusedByBothCommands(t *testing.T) {
	// --yes on clean so the refusal is not merely the confirmation gate
	// declining a non-interactive stdin; the mode check has to be what fails.
	for _, argv := range [][]string{
		{"install", "--dry-run=server"},
		{"clean", "--yes", "--dry-run=server"},
	} {
		t.Run(argv[0]+": --dry-run=server refused, naming the mode that works", func(t *testing.T) {
			root := newRoot(t)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(argv)

			err := root.Execute()
			require.Error(t, err, "an unimplemented mode must fail, not downgrade")
			assert.Contains(t, err.Error(), "server", "the error names what was rejected")
			assert.Contains(t, err.Error(), apcmd.DryRunClient, "the error names the mode that works")
			assert.NotContains(t, out.String(), "would apply", "nothing may be planned for a refused mode")
			assert.NotContains(t, out.String(), "would delete", "nothing may be planned for a refused mode")
		})
	}
}

// TestCleanRefusesABadDryRunBeforeTheConfirmationPrompt: the teardown
// confirmation is the most expensive question oap asks. Asking it and only then
// rejecting the flag spends the user's attention on a run that could never
// proceed — so the mode check runs first, provably, by failing here with no
// --yes and no terminal (which would otherwise fail with the stdin gate's own
// message instead).
func TestCleanRefusesABadDryRunBeforeTheConfirmationPrompt(t *testing.T) {
	root := newRoot(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"clean", "--dry-run=nonsense"})

	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--dry-run", "the mode check is what rejected this")
	assert.NotContains(t, err.Error(), "requires --yes",
		"the stdin gate must not be what the user hears about first")
}
