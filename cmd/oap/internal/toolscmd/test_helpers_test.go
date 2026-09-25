package toolscmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// newToolsCmd builds this package's subtree for a test to drive. `oap tools`
// is the root it hangs off in the assembled CLI, so every argv below reads
// exactly as a user would type it minus that first word.
func newToolsCmd(t *testing.T) *cobra.Command {
	t.Helper()
	return NewCmd(&apcmd.Globals{})
}

// runTools executes `oap tools <argv...>` and returns (stdout+stderr, err).
// Tests that assert on a failure need the error, so this does not use
// aptest.Run, which requires success.
func runTools(t *testing.T, argv ...string) (string, error) {
	t.Helper()
	cmd := newToolsCmd(t)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(argv)
	err := cmd.Execute()
	return buf.String(), err
}
