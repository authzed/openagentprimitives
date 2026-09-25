package cel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	celgo "github.com/google/cel-go/cel"
)

func TestEnv_SimpleCompile(t *testing.T) {
	env, err := Env()
	require.NoError(t, err, "Env")
	ast, issues := env.Compile(`call.subcommand == "x"`)
	require.True(t, issues == nil || issues.Err() == nil, "compile issues: %v", issues)
	require.NotNil(t, ast, "ast")
}

func TestEnv_EvaluateExpression(t *testing.T) {
	env, err := Env()
	require.NoError(t, err, "Env")
	ast, issues := env.Compile(`call.subcommand == "pr view"`)
	require.True(t, issues == nil || issues.Err() == nil, "compile issues: %v", issues)
	prog, err := env.Program(ast)
	require.NoError(t, err, "Program")
	call := map[string]any{
		"subcommand":     "pr view",
		"subcommandPath": []string{"pr", "view"},
		"argv":           []string{"pr", "view"},
		"tail":           []string{},
		"flags":          map[string]any{},
		"positional":     map[string]any{},
		"env":            map[string]string{},
		"cwd":            "",
		"binaryVersion":  "",
		"effects": map[string]any{
			"destructive": false,
			"reads":       []string{},
			"writes":      []string{},
			"network":     map[string]any{"destinations": []string{}},
			"filesystem":  map[string]any{"paths": []string{}},
			"creds":       map[string]any{"required": []string{}, "writes": []string{}},
		},
	}
	out, _, err := prog.Eval(map[string]any{"call": call})
	require.NoError(t, err, "Eval")
	assert.Equal(t, true, out.Value())
	_ = celgo.ObjectType // keep import referenced
}
