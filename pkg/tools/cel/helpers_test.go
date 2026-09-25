package cel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func evalBool(t *testing.T, expr string, call map[string]any) bool {
	t.Helper()
	env, err := Env()
	require.NoError(t, err, "Env")
	ast, iss := env.Compile(expr)
	require.True(t, iss == nil || iss.Err() == nil, "compile %q: %v", expr, iss)
	prog, err := env.Program(ast)
	require.NoError(t, err, "Program")
	out, _, err := prog.Eval(map[string]any{"call": call})
	require.NoError(t, err, "eval %q", expr)
	v, ok := out.Value().(bool)
	require.True(t, ok, "expected bool, got %T", out.Value())
	return v
}

func minimalCall() map[string]any {
	return map[string]any{
		"subcommand": "", "subcommandPath": []string{}, "argv": []string{}, "tail": []string{},
		"flags": map[string]any{}, "positional": map[string]any{},
		"env": map[string]string{}, "cwd": "", "binaryVersion": "",
		"effects": map[string]any{
			"destructive": false, "reads": []string{}, "writes": []string{},
			"network":    map[string]any{"destinations": []string{}},
			"filesystem": map[string]any{"paths": []string{}},
			"creds":      map[string]any{"required": []string{}, "writes": []string{}},
		},
	}
}

// TestHelpers_BuiltinScalarOverloads exercises the standalone builtins
// (path.isUnder, host.of, host.matches, glob.match, semver.satisfies)
// against the minimal `call` context, asserting each expression
// evaluates to the expected bool. These share shape: one expression
// per case, one bool per case.
func TestHelpers_BuiltinScalarOverloads(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want bool
	}{
		{name: "path.isUnder /work/shared/x in /work/shared: true", expr: `path.isUnder("/work/shared/x", "/work/shared")`, want: true},
		{name: "path.isUnder /other not in /work/shared: false", expr: `path.isUnder("/other", "/work/shared")`, want: false},
		{name: "host.of strips https URL: true", expr: `host.of("https://api.github.com/x") == "api.github.com"`, want: true},
		{name: "host.matches exact: true", expr: `host.matches("api.github.com", "api.github.com")`, want: true},
		{name: "host.matches wildcard hit: true", expr: `host.matches("x.internal", "*.internal")`, want: true},
		{name: "host.matches wildcard miss: false", expr: `host.matches("evil.com", "*.internal")`, want: false},
		{name: "glob.match yaml hits yaml: true", expr: `glob.match("*.yaml", "foo.yaml")`, want: true},
		{name: "glob.match yaml miss json: false", expr: `glob.match("*.yaml", "foo.json")`, want: false},
		{name: "semver.satisfies in-range: true", expr: `semver.satisfies("2.53.1", ">=2.40.0 <3.0.0")`, want: true},
		{name: "semver.satisfies out-of-range: false", expr: `semver.satisfies("1.0.0", ">=2.40.0 <3.0.0")`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evalBool(t, tc.expr, minimalCall()))
		})
	}
}

// TestHelpers_CallMemberOverloads exercises call.flag / call.hasFlag /
// call.hasEnv against a call context with specific flag and env state.
// Each case sets the call fields it needs in its setup func, then
// asserts the expression's bool result.
func TestHelpers_CallMemberOverloads(t *testing.T) {
	cases := []struct {
		name  string
		setup func(c map[string]any)
		expr  string
		want  bool
	}{
		{
			name:  "call.flag(repo) returns set value: true",
			setup: func(c map[string]any) { c["flags"] = map[string]any{"repo": "x/y"} },
			expr:  `call.flag("repo") == "x/y"`,
			want:  true,
		},
		{
			name:  "call.flag(missing) returns empty: true",
			setup: func(c map[string]any) { c["flags"] = map[string]any{"repo": "x/y"} },
			expr:  `call.flag("missing") == ""`,
			want:  true,
		},
		{
			name:  "call.hasFlag(a) when flag present: true",
			setup: func(c map[string]any) { c["flags"] = map[string]any{"a": "x"} },
			expr:  `call.hasFlag("a")`,
			want:  true,
		},
		{
			name:  "call.hasFlag(b) when flag absent: false",
			setup: func(c map[string]any) { c["flags"] = map[string]any{"a": "x"} },
			expr:  `call.hasFlag("b")`,
			want:  false,
		},
		{
			name:  "call.hasEnv(HOME) when env present: true",
			setup: func(c map[string]any) { c["env"] = map[string]string{"HOME": "/root"} },
			expr:  `call.hasEnv("HOME")`,
			want:  true,
		},
		{
			name:  "call.hasEnv(NOPE) when env absent: false",
			setup: func(c map[string]any) { c["env"] = map[string]string{"HOME": "/root"} },
			expr:  `call.hasEnv("NOPE")`,
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := minimalCall()
			tc.setup(c)
			assert.Equal(t, tc.want, evalBool(t, tc.expr, c))
		})
	}
}

// TestCallMemberOverloads_NonNativeReceiverErrors pins the failure direction of
// the call.* member helpers when their receiver is NOT the native
// map[string]any that the toolspec validator binds.
//
// A CEL map literal type-checks against the declared map(string, dyn) receiver
// AND passes cel-go's runtime type guard — that guard compares CEL types, not
// Go representations — so the binding really is invoked, with a receiver whose
// Value() is map[ref.Val]ref.Val. Discarding the assertion's ok therefore
// produced a nil map and, from there, the helper's ZERO VALUE: hasFlag
// reported false for a flag plainly present in the literal.
//
// For an author-written deny rule shaped like `<call>.hasFlag("force")` that is
// fail-OPEN — the constraint quietly stops denying and nothing is raised. So
// each case asserts an error, not a bool the caller has no reason to trust.
func TestCallMemberOverloads_NonNativeReceiverErrors(t *testing.T) {
	cases := []struct {
		name string
		expr string
		// silentZero is what the pre-fix code returned instead of erroring,
		// recorded here so the fail-open this test closes is named in the
		// failure message rather than left to the reader.
		silentZero any
	}{
		{
			name:       "hasFlag on a map literal: errors, rather than reporting a present flag absent",
			expr:       `{"flags": {"force": "true"}}.hasFlag("force")`,
			silentZero: false,
		},
		{
			name:       "flag on a map literal: errors, rather than reporting a present flag empty",
			expr:       `{"flags": {"file": "/etc/passwd"}}.flag("file")`,
			silentZero: "",
		},
		{
			name:       "hasEnv on a map literal: errors, rather than reporting a present env var absent",
			expr:       `{"env": {"TOKEN": "x"}}.hasEnv("TOKEN")`,
			silentZero: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := Env()
			require.NoError(t, err, "Env")
			ast, iss := env.Compile(tc.expr)
			require.True(t, iss == nil || iss.Err() == nil,
				"a map-literal receiver must still type-check — that is what makes this reachable: %v", iss)
			prog, err := env.Program(ast)
			require.NoError(t, err, "Program")

			_, _, err = prog.Eval(map[string]any{"call": minimalCall()})
			require.Error(t, err,
				"a non-native receiver must error; it silently returned %#v", tc.silentZero)
			assert.ErrorContains(t, err, "receiver must be a call map")
		})
	}
}

// TestCallMemberOverloads_NativeReceiverStillWorks is the other half of the
// contract above: the receiver the validator actually binds — a native
// map[string]any from callAsMap — must keep evaluating, not start erroring.
func TestCallMemberOverloads_NativeReceiverStillWorks(t *testing.T) {
	call := minimalCall()
	call["flags"] = map[string]any{"force": "true", "file": "/etc/hosts"}
	call["env"] = map[string]string{"TOKEN": "x"}

	assert.True(t, evalBool(t, `call.hasFlag("force")`, call))
	assert.False(t, evalBool(t, `call.hasFlag("absent")`, call))
	assert.True(t, evalBool(t, `call.flag("file") == "/etc/hosts"`, call))
	assert.True(t, evalBool(t, `call.flag("absent") == ""`, call), "an unset flag is still the empty string, not an error")
	assert.True(t, evalBool(t, `call.hasEnv("TOKEN")`, call))
	assert.False(t, evalBool(t, `call.hasEnv("ABSENT")`, call))
}
