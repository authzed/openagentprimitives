package validator

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/redact"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// The value every test here declares sensitive. Distinctive enough that a
// substring hit in the rendered Decision can only be this value.
const sf11Secret = "s3cr3t-sf11-value"

// variadicToolkit is simpleToolkit plus one sensitive env var and a trailing
// variadic slot. The variadic is the point: `stringList` binds to []string
// (parser/flags.go, parser/declarative.go), which is the shape the redaction
// walk used to hand back untouched.
func variadicToolkit(t *testing.T) *toolkit.Toolkit {
	t.Helper()
	tk := simpleToolkit()
	tk.Env.Allowed = []toolkit.EnvVar{{Name: "TOKEN", Sensitive: true, Description: "auth token"}}
	tk.Subcommands[0].Positional = []toolkit.Positional{{Name: "words", Type: "stringList"}}
	return tk
}

// parsedJSON renders the Decision's parsed view the way every consumer of
// Decision.Parsed eventually does — `oap tools toolspec explain` prints it, the
// MCP dispatch path marshals its sibling into an audit record.
func parsedJSON(t *testing.T, d *Decision) string {
	t.Helper()
	require.NotNil(t, d.Parsed, "Parsed must be populated once the parse succeeds")
	b, err := json.Marshal(d.Parsed)
	require.NoError(t, err, "marshal Parsed")
	return string(b)
}

// TestCheck_SensitiveValueInAStringListSlotIsRedacted pins the fail-open
// default in the redaction walk. The walk handled string, map[string]any and
// []any and returned everything else — including []string — unchanged, so a
// value the operator declared sensitive rode out of the validator raw in a
// slot the walk had already "covered".
//
// The binding is driven through the real parser (a declared `stringList`
// positional and a declared `stringList` flag), not a hand-built []string:
// the defect is only reachable because that is what the parser produces.
func TestCheck_SensitiveValueInAStringListSlotIsRedacted(t *testing.T) {
	t.Run("variadic positional: the []string slot must be masked, not passed through", func(t *testing.T) {
		tk := variadicToolkit(t)
		d, err := Check(tk, simpleSpec(), Invocation{
			Command: "t", Argv: []string{"say", sf11Secret}, BinaryVersion: "1.5.0",
			Env: map[string]string{"TOKEN": sf11Secret},
		})
		require.NoError(t, err, "Check must not report an internal error")

		assert.NotContains(t, parsedJSON(t, d), sf11Secret,
			"a declared-sensitive value bound to a stringList positional must not survive in Parsed")

		words, ok := d.Parsed.Positional["words"].([]string)
		require.True(t, ok, "the variadic slot keeps its []string type; got %T", d.Parsed.Positional["words"])
		require.Len(t, words, 1, "one value was bound")
		assert.Contains(t, words[0], "<redacted", "it must be replaced by a token, not dropped")
	})

	t.Run("stringList flag: every repeated value is registered and masked", func(t *testing.T) {
		tk := variadicToolkit(t)
		tk.Subcommands[0].Flags = []toolkit.Flag{
			{Long: "header", Type: "stringList", Sensitive: true, Description: "auth header"},
		}
		const second = "s3cr3t-sf11-second"
		d, err := Check(tk, simpleSpec(), Invocation{
			Command:       "t",
			Argv:          []string{"say", "--header", sf11Secret, "--header", second},
			BinaryVersion: "1.5.0",
		})
		require.NoError(t, err, "Check must not report an internal error")

		// A walk cannot mask what was never registered: a sensitive flag whose
		// value is a []string was skipped by registerSensitiveFlagValues, which
		// only recorded string-typed values.
		kinds := map[string]string{}
		for _, desc := range d.Redactions {
			kinds[desc.Name] = desc.Kind
		}
		assert.Equal(t, "flag", kinds["header"],
			"a sensitive stringList flag must register its values; redactions=%+v", d.Redactions)

		got := parsedJSON(t, d)
		assert.NotContains(t, got, sf11Secret, "first --header value must be masked")
		assert.NotContains(t, got, second, "second --header value must be masked")
	})
}

// TestCheck_SensitiveValueInTailAndArgvIsRedacted pins the half-walked scrub:
// finalize walked Flags and Positional and skipped Tail and Argv, so the same
// value appeared masked in one field of the Decision and verbatim two fields
// later. Only this layer can fix it — by the time a consumer holds the
// Decision the raw value is unrecoverable from the masked fields.
func TestCheck_SensitiveValueInTailAndArgvIsRedacted(t *testing.T) {
	tk := variadicToolkit(t)
	d, err := Check(tk, simpleSpec(), Invocation{
		Command: "t", Argv: []string{"say", "--", sf11Secret}, BinaryVersion: "1.5.0",
		Env: map[string]string{"TOKEN": sf11Secret},
	})
	require.NoError(t, err, "Check must not report an internal error")
	require.NotNil(t, d.Parsed, "Parsed must be populated once the parse succeeds")

	require.NotEmpty(t, d.Parsed.Tail, "this fixture puts the value in the post-`--` tail")
	require.NotEmpty(t, d.Parsed.Argv, "Argv is the invocation as typed")
	assert.NotContains(t, d.Parsed.Tail, sf11Secret, "Parsed.Tail must be scrubbed like every other field")
	assert.NotContains(t, d.Parsed.Argv, sf11Secret, "Parsed.Argv must be scrubbed like every other field")
	assert.NotContains(t, parsedJSON(t, d), sf11Secret, "no field of the parsed view may echo it")
}

// TestCheck_SensitivePositionalDeclarationIsHonored covers the third way a
// value is declared sensitive. spec.Sensitive.Positional is a user-facing
// field on the SpiceboxToolspec CRD (ToolspecSensitive.Positional) that was
// read by nothing: an operator who declared a positional sensitive got no
// redaction anywhere, silently.
func TestCheck_SensitivePositionalDeclarationIsHonored(t *testing.T) {
	tk := simpleToolkit()
	tk.Subcommands[0].Positional = []toolkit.Positional{{Name: "target", Type: "string"}}
	sp := simpleSpec()
	sp.Sensitive = spec.Sensitive{Positional: []string{"target"}}

	d, err := Check(tk, sp, Invocation{
		Command: "t", Argv: []string{"say", sf11Secret}, BinaryVersion: "1.5.0",
	})
	require.NoError(t, err, "Check must not report an internal error")

	names := map[string]string{}
	for _, desc := range d.Redactions {
		names[desc.Name] = desc.Kind
	}
	assert.Equal(t, "positional", names["target"],
		"a spec-declared sensitive positional must register; redactions=%+v", d.Redactions)
	assert.NotContains(t, parsedJSON(t, d), sf11Secret, "the declared-sensitive positional must be masked")
}

// TestParsedCall_EveryStringBearingFieldIsScrubbed is the drift guard. It
// reflects over ParsedCall, plants a distinct marker in every exported
// string-bearing field, and requires that none of them survives finalize.
//
// Adding a field to ParsedCall therefore either gets covered by the walk or
// fails here — which is the point. The previous shape (a scrub that named two
// of the struct's fields) could not fail when a third and fourth were added,
// and did not.
func TestParsedCall_EveryStringBearingFieldIsScrubbed(t *testing.T) {
	pc := &ParsedCall{}
	rv := reflect.ValueOf(pc).Elem()
	rt := rv.Type()

	r := redact.New()
	markers := map[string]string{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		marker := fmt.Sprintf("s3cr3t-sf11-%s", f.Name)
		markers[f.Name] = marker
		r.RegisterSensitive(marker, redact.Descriptor{Description: f.Name, Kind: "env", Name: f.Name})

		switch {
		case f.Type.Kind() == reflect.String:
			rv.Field(i).SetString(marker)
		case f.Type == reflect.TypeOf([]string(nil)):
			rv.Field(i).Set(reflect.ValueOf([]string{marker}))
		case f.Type == reflect.TypeOf(map[string]any(nil)):
			rv.Field(i).Set(reflect.ValueOf(map[string]any{"slot": marker}))
		default:
			t.Fatalf("ParsedCall.%s has shape %s that this drift guard cannot plant a value in — "+
				"teach the guard to fill it AND confirm core.Finalize's walk reaches it",
				f.Name, f.Type)
		}
	}
	require.NotEmpty(t, markers, "ParsedCall must have exported fields to check")

	d := &Decision{Parsed: pc}
	finalize(d, r)

	b, err := json.Marshal(d.Parsed)
	require.NoError(t, err, "marshal Parsed")
	for name, marker := range markers {
		assert.NotContains(t, string(b), marker,
			"ParsedCall.%s carried a registered sensitive value out of finalize unscrubbed", name)
	}
}

// TestCheck_ParsedKeepsItsTypesForStructuredConsumers is the other half of the
// walk's contract: consumers read Decision.Parsed structurally (the dash-dash
// regression test asserts a []string positional; the MCP dispatch path
// marshals its sibling into an audit record), so scrubbing must not reshape a
// value that carries no secret.
func TestCheck_ParsedKeepsItsTypesForStructuredConsumers(t *testing.T) {
	tk := simpleToolkit()
	tk.Subcommands[0].Positional = []toolkit.Positional{{Name: "words", Type: "stringList"}}
	tk.Subcommands[0].Flags = []toolkit.Flag{
		{Long: "count", Type: "int"},
		{Long: "verbose", Type: "bool"},
		{Long: "name", Type: "string"},
	}
	d, err := Check(tk, simpleSpec(), Invocation{
		Command:       "t",
		Argv:          []string{"say", "--count", "7", "--verbose", "--name", "demo", "alpha", "beta"},
		BinaryVersion: "1.5.0",
	})
	require.NoError(t, err, "Check must not report an internal error")
	require.NotNil(t, d.Parsed, "Parsed must be populated once the parse succeeds")

	assert.Equal(t, []string{"alpha", "beta"}, d.Parsed.Positional["words"], "variadic slot stays []string")
	assert.Equal(t, int64(7), d.Parsed.Flags["count"], "an int flag stays int64")
	assert.Equal(t, true, d.Parsed.Flags["verbose"], "a bool flag stays bool")
	assert.Equal(t, "demo", d.Parsed.Flags["name"], "a string flag keeps its value when nothing is sensitive")
}
