package settingscmd

import (
	"bytes"
	"testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// runSettings executes `oap settings <argv...>` against this package's own
// subtree and returns everything it wrote plus the RunE error. Tests that
// stub DynamicFactory need the error, so this does not use aptest.Run (which
// requires success).
func runSettings(t *testing.T, argv ...string) (string, error) {
	t.Helper()
	return runSettingsG(t, &apcmd.Globals{}, argv...)
}

// runSettingsG is runSettings over a caller-supplied Globals — used by the
// wizard tests that need a fake-backed Bundle (the --defaults path reads the
// existing ClusterAgentSettings through g.Bundle().Controller before it
// composes, so a nil BundleFn would try to reach a real cluster).
func runSettingsG(t *testing.T, g *apcmd.Globals, argv ...string) (string, error) {
	t.Helper()
	cmd := NewCmd(g)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(argv)
	err := cmd.Execute()
	return buf.String(), err
}
