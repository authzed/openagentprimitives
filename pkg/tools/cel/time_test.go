package cel

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestNow_RegisteredInBothScopes asserts now() is discoverable in the
// toolspec env (call-shaped) AND the MCP env (args-shaped). The LLM
// authoring prompts splice HelperDocs in both contexts; the runtime
// evaluator runs against whichever env matches the constraint kind.
func TestNow_RegisteredInBothScopes(t *testing.T) {
	assert.Contains(t, names(HelpersFor(ScopeToolspec)), "now", "now missing from ScopeToolspec helpers")
	assert.Contains(t, names(HelpersFor(ScopeMCP)), "now", "now missing from ScopeMCP helpers")
}

// TestNow_ToolspecEnv_Eval verifies now() compiles and evaluates in the
// toolspec env (where `call` is the variable). The returned timestamp
// must be within a small window of wall-clock — proves the binding
// actually calls time.Now() at eval time, not at compile time.
func TestNow_ToolspecEnv_Eval(t *testing.T) {
	env, err := Env()
	require.NoError(t, err, "Env")
	ast, iss := env.Compile(`now()`)
	require.True(t, iss == nil || iss.Err() == nil, "compile now(): %v", iss)
	prog, err := env.Program(ast)
	require.NoError(t, err, "program")
	before := time.Now()
	out, _, err := prog.Eval(map[string]any{"call": map[string]any{}})
	require.NoError(t, err, "eval")
	after := time.Now()

	got, ok := out.Value().(time.Time)
	require.True(t, ok, "now() returned %T, want time.Time", out.Value())
	assert.False(t, got.Before(before), "now() = %v; expected >= %v", got, before)
	assert.False(t, got.After(after), "now() = %v; expected <= %v", got, after)
}

// TestNow_MCPEnv_Eval verifies now() compiles and evaluates in the MCP
// env (where `args` is the variable). Uses a duration comparison to
// exercise the full timestamp-arithmetic path the LLM is likely to
// generate ("created within the last week").
func TestNow_MCPEnv_Eval(t *testing.T) {
	env, err := MCPEnv()
	require.NoError(t, err, "MCPEnv")
	// Compare now() against a fixed past timestamp; the expression must
	// type-check and return a bool. The actual value isn't asserted here
	// — just that the env supports timestamp arithmetic.
	ast, iss := env.Compile(`now() - timestamp("2020-01-01T00:00:00Z") > duration("1h")`)
	require.True(t, iss == nil || iss.Err() == nil, "compile: %v", iss)
	prog, err := env.Program(ast)
	require.NoError(t, err, "program")
	argsStruct, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err, "structpb")
	out, _, err := prog.Eval(map[string]any{"args": argsStruct})
	require.NoError(t, err, "eval")
	assert.Equal(t, true, out.Value(), "expected true (now > 2020)")
}
