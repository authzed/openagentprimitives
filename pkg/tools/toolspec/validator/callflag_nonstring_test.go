package validator

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// call.flag() on a non-string flag returned "" — the same value it returns for
// an ABSENT flag — so a spec's `call.flag(x) == ""`, meant as "the flag is not
// set", matched a bool `true` exactly. Against a read-only-one-namespace spec, a
// call passing --all-namespaces was allowed: every Secret in the cluster.
//
// The fix makes call.flag() ERROR on a non-string, and checkConstraints
// propagates an eval error (validator.go: `return nil, err`), so the whole
// validation fails closed. The pkg/tools/cel test proves the primitive errors;
// this proves the VALIDATOR pipeline propagates that error rather than treating
// the constraint as satisfied — the tier the golden cases (which use hasFlag)
// never reach.
//
// Load-bearing: revert the engine fix and call.flag("all-namespaces") renders
// "", the constraint is `"" == ""` → true → Check returns (allow=true, nil), and
// the require.Error below fails. This cannot be a golden case: the golden
// harness require.NoError(t, err)s, and here Check's whole job is to error.
func TestCallFlag_NonStringPropagatesFailClosed(t *testing.T) {
	toolDir := filepath.Join("testdata", "kubectl")
	tk, err := toolkit.Load(filepath.Join(toolDir, "toolkit.yaml"))
	require.NoError(t, err, "load kubectl toolkit")
	sp, err := spec.Load(filepath.Join(toolDir, "spec-callflag-nonstring.yaml"))
	require.NoError(t, err, "load call.flag non-string spec")

	// --all-namespaces is a bool flag; call.flag() on it must error, not render "".
	_, err = Check(tk, sp, Invocation{
		Command:       "kubectl",
		Argv:          []string{"get", "secrets", "--all-namespaces"},
		BinaryVersion: "1.30.0",
	})
	require.Error(t, err,
		"call.flag() on a non-string flag must error and the validator must propagate it, "+
			"failing closed; rendering \"\" instead makes the constraint `\"\" == \"\"` → true and allows the call")
	assert.ErrorContains(t, err, "not a string")
	assert.ErrorContains(t, err, "constraints[0]",
		"the failure must be attributed to the constraint that evaluated call.flag(), not swallowed")
}
