package cel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolspecEnvExposesConfigRoot(t *testing.T) {
	prog, err := Compile(`config.allowedRepos.exists(r, glob.match(r, call.resourceId))`)
	require.NoError(t, err, "config root and glob.match must both compile in the toolspec env")

	ok, err := EvalBool(prog,
		map[string]any{"resourceId": "demo-org/demo-repo"},
		map[string]any{"allowedRepos": []any{"demo-org/*"}},
	)
	require.NoError(t, err)
	assert.True(t, ok, "demo-org/* should match demo-org/demo-repo")

	ok, err = EvalBool(prog,
		map[string]any{"resourceId": "other-org/x"},
		map[string]any{"allowedRepos": []any{"demo-org/*"}},
	)
	require.NoError(t, err)
	assert.False(t, ok, "demo-org/* must not match other-org/x")
}

func TestEvalBoolFailsClosedOnMissingConfigKey(t *testing.T) {
	prog, err := Compile(`config.allowedRepos.exists(r, r == call.resourceId)`)
	require.NoError(t, err)
	// config present but the key absent → CEL no-such-key error → caller denies.
	_, err = EvalBool(prog, map[string]any{"resourceId": "a/b"}, map[string]any{})
	assert.Error(t, err, "a missing config key must surface as an error (fail-closed), not false")
}
