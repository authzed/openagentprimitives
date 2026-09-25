package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestSpicedbUserID_MatchesIdentityCanonicalize(t *testing.T) {
	prg, err := CompileString(`spicedb_user_id("Carol@example.com")`)
	require.NoError(t, err, "compile")
	got, err := EvalString(prg, nil)
	require.NoError(t, err, "eval")
	want, err := identity.EmailReference("Carol@example.com").Canonical()
	require.NoError(t, err)
	// identity boundary: spicedb_user_id returns a CEL types.String; the test
	// compares against the .String() form of the expected canonical.
	assert.Equal(t, want.String(), got, "spicedb_user_id should match identity.Principal.Canonical")
}

func TestSpicedbUserID_EmptyEmailIsError(t *testing.T) {
	prg, err := CompileString(`spicedb_user_id("")`)
	require.NoError(t, err, "compile")
	_, err = EvalString(prg, nil)
	require.Error(t, err, "empty email should produce an error")
}
