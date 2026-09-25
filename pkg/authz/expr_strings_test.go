package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// Toolkit authors write resourceIDExpr to derive an authorizable id from a
// call's arguments, and the arguments are strings that need taking apart: a gh
// endpoint `repos/OWNER/NAME/pulls` carries the repository in its path, and
// startsWith alone cannot extract it.
//
// The env registered only the variables and spicedb_user_id, so `split` was an
// `undeclared reference` — the gh api guard compiled to nothing and could never
// authorize a call, not even a well-formed one. It failed CLOSED, so it was not
// a hole; it was a guard that could only ever say no, which is indistinguishable
// from a correct guard if the only test asserts that bad input is refused.
func TestCEL_providesStringHelpersForArgumentParsing(t *testing.T) {
	cases := []struct {
		name string
		expr string
		args map[string]any
		want string
	}{
		{
			name: "split extracts a path segment",
			expr: `args.endpoint.split("/")[1]`,
			args: map[string]any{"endpoint": "repos/demo-org/demo-repo/pulls"},
			want: "demo-org",
		},
		{
			name: "lowerAscii normalizes case",
			expr: `args.name.lowerAscii()`,
			args: map[string]any{"name": "Demo-Org"},
			want: "demo-org",
		},
		{
			name: "join rebuilds a scoped id",
			expr: `[args.endpoint.split("/")[1], args.endpoint.split("/")[2]].join("/")`,
			args: map[string]any{"endpoint": "repos/demo-org/demo-repo"},
			want: "demo-org/demo-repo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prg, err := authz.CompileString(tc.expr)
			require.NoError(t, err, "expression must compile against the shared env")
			got, err := authz.EvalString(prg, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The extension must not smuggle in anything that can reach outside the call's
// own arguments. CEL stays total and has no I/O, but a spot-check that an
// unknown identifier is still a COMPILE error is what keeps "we added a library"
// from quietly meaning "we added an escape hatch".
func TestCEL_stillRejectsAnUndeclaredReference(t *testing.T) {
	_, err := authz.CompileString(`readFile("/etc/passwd")`)
	assert.Error(t, err, "an unknown function must fail at compile, not at eval")
}
