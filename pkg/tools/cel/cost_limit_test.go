package cel_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ctcel "github.com/authzed/openagentprimitives/pkg/tools/cel"
)

// A toolspec constraint is CEL, and it was compiled with no runtime cost limit
// at all. Toolspec constraints are evaluated in the OPERATOR during ToolCall
// reconciliation, which is serialized to one worker — so a single expensive
// expression stalls ToolCall reconciliation cluster-wide, with no error and no
// terminal condition. The repo already documents that exact failure shape for a
// different cause.
//
// The expression author is not always a tenant admin either: specs are
// LLM-generated from documents that may be hostile, and a merely quadratic
// constraint over a model-supplied list gets there by accident rather than by
// malice.
//
// A cost limit turns "runs forever" into "returns an error", which is the whole
// finding. It needs no context plumbing, so it holds on every evaluation path
// including the ones that have no deadline to interrupt.

// quadraticOverArgs is a constraint of exactly the accidental shape: a nested
// comprehension over a caller-supplied list. Nothing about it looks hostile.
const quadraticOverArgs = `call.items.all(a, call.items.all(b, call.items.all(c, size(a) + size(b) + size(c) >= 0)))`

func bigList(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "item-value-"+strings.Repeat("x", 8))
	}
	return out
}

func TestEvalBool_RunawayExpressionIsBounded(t *testing.T) {
	prog, err := ctcel.Compile(quadraticOverArgs)
	require.NoError(t, err, "the expression compiles — cost is a RUNTIME property")

	_, err = ctcel.EvalBool(prog, map[string]any{"items": bigList(120)}, nil)

	require.Error(t, err, "an expression that blows the budget must return, not run")
	assert.Contains(t, strings.ToLower(err.Error()), "cost",
		"the error should name the budget, so an operator reading a failed ToolCall knows why")
}

// The counterweight: an ordinary constraint is nowhere near the budget and must
// keep evaluating exactly as before. A limit set too low would turn this fix
// into an outage of its own, and it would look like a flaky constraint rather
// than a deliberate cap.
func TestEvalBool_OrdinaryConstraintIsUnaffected(t *testing.T) {
	prog, err := ctcel.Compile(`call.flags.exists(f, f == "--json") && size(call.args) < 10`)
	require.NoError(t, err)

	got, err := ctcel.EvalBool(prog, map[string]any{
		"flags": []any{"--json", "--limit"},
		"args":  []any{"repos"},
	}, nil)

	require.NoError(t, err, "a normal constraint must not be clipped by the budget")
	assert.True(t, got)
}
