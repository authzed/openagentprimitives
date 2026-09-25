package cel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// call.flag() on a NON-STRING flag returned "" — the same value it returns for
// a flag that is absent — so a constraint could not tell "the flag is not set"
// from "the flag is set to true".
//
// That inverted the invariant the toolkit type documents: "a value-typed
// comparison against `true` fails the constraint closed rather than matching a
// fabricated zero value." Comparing against `true` does fail closed. Comparing
// against "" — the shape an author reaches for, and the shape actually shipped
// in three specs — matched the fabricated zero exactly.
//
// The concrete break: a bool flag is stored as a Go `true` by the parser, so
//
//	!call.hasFlag("all-namespaces") || call.flag("all-namespaces") == ""
//
// evaluated `false || true` for EVERY call, present or absent. Against a spec
// whose stated intent is read-only access restricted to one namespace, a call
// naming the permitted namespace AND passing the cluster-wide flag was allowed
// — every Secret in the cluster, from a spec that says it cannot leave one
// namespace. The class is wider than bool: int and stringList rendered as ""
// the same way.
//
// Erroring is the answer callMap already reaches for on a bad receiver, and
// for the same stated reason: the validator propagates an eval error, so the
// constraint fails closed instead of quietly passing.
func TestCallFlag_NonStringValueErrorsRatherThanRenderingEmpty(t *testing.T) {
	cases := []struct {
		name string
		val  any
		// what the caller silently got before, spelled out so a regression
		// says which fabricated value came back.
		silentZero string
	}{
		{name: "a bool flag (--all-namespaces, --force)", val: true, silentZero: `""`},
		{name: "an int flag (--limit)", val: int64(5), silentZero: `""`},
		{name: "a stringList flag (repeated -f)", val: []any{"a", "b"}, silentZero: `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := minimalCall()
			call["flags"] = map[string]any{"x": tc.val}

			env, err := Env()
			require.NoError(t, err, "Env")
			ast, iss := env.Compile(`call.flag("x") == ""`)
			require.True(t, iss == nil || iss.Err() == nil, "compile: %v", iss)
			prog, err := env.Program(ast)
			require.NoError(t, err, "Program")

			_, _, err = prog.Eval(map[string]any{"call": call})
			require.Error(t, err,
				"a non-string flag must error; it silently rendered %s, which is also what an ABSENT flag renders",
				tc.silentZero)
			assert.ErrorContains(t, err, "not a string")
		})
	}
}

// The other half: an ABSENT flag must keep returning "", because that is the
// only way an author writes "this flag was not passed" and every existing spec
// depends on it. Erroring here instead would fail closed on legitimate calls.
func TestCallFlag_AnAbsentFlagIsStillTheEmptyString(t *testing.T) {
	call := minimalCall()
	call["flags"] = map[string]any{"other": "v"}

	assert.True(t, evalBool(t, `call.flag("x") == ""`, call),
		"an unset flag is the empty string; only a SET non-string value is unanswerable")
}
