package validator_test

import (
	"encoding/json"
	"strconv"
	"testing"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheck_RedactsSensitiveFieldsFromReasonAndTrace(t *testing.T) {
	const secret = "SECRET-LEAKED-VALUE"
	sp := &mcpspec.Spec{
		Name: "x", Version: "1",
		Server: mcpspec.Server{URL: "https://e.x/mcp", Transport: mcpspec.TransportStreamableHTTP},
		Tools: []mcpspec.Tool{{
			Name: "alpha",
			Args: mcpspec.Args{
				SensitiveFields: []string{"apiKey"},
				Constraints: []toolspec.Constraint{{
					CEL:     `args.apiKey == "expected"`,
					Message: "apiKey did not match",
				}},
			},
		}},
	}
	d, err := validator.Check(sp, validator.Invocation{
		ToolName: "alpha",
		Args:     map[string]any{"apiKey": secret},
	})
	require.NoError(t, err, "Check")
	require.False(t, d.Allow, "expected deny")

	// The sensitive value must NOT appear verbatim in any user-visible field.
	assert.NotContains(t, d.Reason, secret, "Reason leaked the sensitive value")
	for i, tr := range d.Trace {
		assert.NotContains(t, tr.Detail, secret, "Trace["+strconv.Itoa(i)+"].Detail leaked sensitive value")
		assert.NotContains(t, tr.Rule, secret, "Trace["+strconv.Itoa(i)+"].Rule leaked sensitive value")
	}
	if d.Parsed != nil {
		for k, v := range d.Parsed.Args {
			if s, ok := v.(string); ok {
				assert.NotContains(t, s, secret, "ParsedArgs.Args["+k+"] leaked sensitive value")
			}
		}
	}
	// And the redactor's descriptor map should be populated.
	assert.NotEmpty(t, d.Redactions, "expected at least one redaction descriptor")
}

// TestCheck_RedactsNonScalarSensitiveValues guards the fail-closed
// contract: a field DECLARED sensitive must never appear in plaintext in
// the emitted, serialized Decision — regardless of whether its value is a
// scalar, a nested object, an array, or a number. The audit log records
// Decision.Parsed via json.Marshal, so the realistic leak surface is the
// JSON serialization of Parsed.Args.
func TestCheck_RedactsNonScalarSensitiveValues(t *testing.T) {
	const (
		nestedSecret = "s3cr3t-nested-token"
		arraySecret  = "s3cr3t-array-element"
		numberSecret = 8675309 // a "secret" account/PIN-shaped number
	)
	cases := []struct {
		name    string
		field   string
		value   any
		secrets []string // byte sequences that must NOT survive
	}{
		{
			name:    "nested object value",
			field:   "creds",
			value:   map[string]any{"token": nestedSecret},
			secrets: []string{nestedSecret},
		},
		{
			name:    "array containing a secret string",
			field:   "tokens",
			value:   []any{"benign", arraySecret},
			secrets: []string{arraySecret},
		},
		{
			name:    "json number value",
			field:   "pin",
			value:   float64(numberSecret),
			secrets: []string{strconv.Itoa(numberSecret)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &mcpspec.Spec{
				Name: "x", Version: "1",
				Server: mcpspec.Server{URL: "https://e.x/mcp", Transport: mcpspec.TransportStreamableHTTP},
				Tools: []mcpspec.Tool{{
					Name: "alpha",
					Args: mcpspec.Args{SensitiveFields: []string{tc.field}},
				}},
			}
			d, err := validator.Check(sp, validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{tc.field: tc.value},
			})
			require.NoError(t, err, "Check")

			require.NotNil(t, d.Parsed, "Parsed must be populated")
			raw, err := json.Marshal(d.Parsed.Args)
			require.NoError(t, err, "marshal Parsed.Args (the audit surface)")
			for _, s := range tc.secrets {
				assert.NotContains(t, string(raw), s,
					"declared-sensitive value leaked verbatim in serialized Parsed.Args: %s", string(raw))
			}
		})
	}
}

// TestCheck_MalformedSensitiveFieldPathWarnsAndStillFailsClosed pins the
// surfacing half of the malformed-path contract. A sensitiveFields entry that
// does not parse is an authoring bug in the spec; the redactor fails closed onto
// the enclosing value, so nothing leaks and the verdict is unaffected — but the
// declared path never resolved, and without the Warning the only symptom would
// be a whole argument object collapsing into one token for no visible reason.
func TestCheck_MalformedSensitiveFieldPathWarnsAndStillFailsClosed(t *testing.T) {
	const nestedSecret = "s3cr3t-nested-token"

	// checkWithSensitiveField runs the same invocation against a spec that
	// declares exactly one sensitive path, so a malformed path's Decision can be
	// compared against the well-formed control.
	checkWithSensitiveField := func(t *testing.T, path string) *validator.Decision {
		t.Helper()
		sp := &mcpspec.Spec{
			Name: "x", Version: "1",
			Server: mcpspec.Server{URL: "https://e.x/mcp", Transport: mcpspec.TransportStreamableHTTP},
			Tools: []mcpspec.Tool{{
				Name: "alpha",
				Args: mcpspec.Args{SensitiveFields: []string{path}},
			}},
		}
		d, err := validator.Check(sp, validator.Invocation{
			ToolName: "alpha",
			Args:     map[string]any{"creds": map[string]any{"token": nestedSecret}},
		})
		require.NoError(t, err, "an authoring bug in sensitiveFields must not make a verdict impossible")
		require.NotNil(t, d, "Check must return a Decision")
		return d
	}

	control := checkWithSensitiveField(t, "creds")
	require.Empty(t, control.Warnings, "the well-formed control must warn about nothing")

	cases := []struct {
		name string
		path string
	}{
		{name: "non-integer array index: warns, enclosing value still redacted", path: "creds[x].token"},
		{name: "unterminated bracket: warns, enclosing value still redacted", path: "creds[0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := checkWithSensitiveField(t, tc.path)
			assert.Equal(t, control.Allow, d.Allow, "the malformed declaration must not change the verdict")

			require.NotNil(t, d.Parsed, "Parsed must be populated")
			raw, err := json.Marshal(d.Parsed.Args)
			require.NoError(t, err, "marshal Parsed.Args (the audit surface)")
			assert.NotContains(t, string(raw), nestedSecret,
				"fail-closed: the enclosing value must still be redacted, got %s", string(raw))

			require.Len(t, d.Warnings, 1, "the unhonored declaration must be surfaced, got %#v", d.Warnings)
			assert.Equal(t, validator.WarnSensitiveFieldPathMalformed, d.Warnings[0].Kind)
			assert.Contains(t, d.Warnings[0].Message, tc.path, "the warning must name the offending path")
			assert.Contains(t, d.Warnings[0].Message, "alpha", "the warning must name the tool that declared it")
		})
	}
}
