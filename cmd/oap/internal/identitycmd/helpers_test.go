package identitycmd

import (
	"bytes"
	"testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// runIdentity executes `oap identity <argv...>` against this package's own
// subtree and returns (stdout+stderr, err). Tests that assert on a refusal
// need the error, so this does not use aptest.Run, which requires success.
func runIdentity(t *testing.T, argv ...string) (string, error) {
	t.Helper()
	cmd := NewCmd(&apcmd.Globals{})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(argv)
	err := cmd.Execute()
	return buf.String(), err
}
