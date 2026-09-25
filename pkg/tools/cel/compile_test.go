package cel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompile_OK(t *testing.T) {
	p, err := Compile(`call.subcommand == "x"`)
	require.NoError(t, err, "Compile")
	require.NotNil(t, p, "expected non-nil program")
}

func TestCompile_Error(t *testing.T) {
	_, err := Compile(`call.subcommand ==`)
	require.Error(t, err, "expected compile error")
	assert.Contains(t, err.Error(), "compile", "err should mention compile")
}

func TestEval_Bool(t *testing.T) {
	p, err := Compile(`call.subcommand == "x"`)
	require.NoError(t, err, "Compile")
	c := minimalCall()
	c["subcommand"] = "x"
	got, err := EvalBool(p, c, nil)
	require.NoError(t, err, "EvalBool")
	assert.True(t, got)
}
