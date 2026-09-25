package redact

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A malformed declared path — an unterminated bracket, or a bracket holding
// something that is not an integer — parses only as far as the bad bracket.
// The segments that DID parse address a strict ANCESTOR of the value the spec
// author meant to protect, so the redaction that happens is broader than
// declared, not narrower.
//
// Broader is the safe direction and must stay that way: refusing to redact
// would turn a wrong-but-safe audit record into a cleartext secret. What must
// NOT stay is the silence — the descriptor the Redactor emits currently claims
// the declared path was resolved, so the audit record asserts something that
// never happened and an operator has nothing to grep for.

// malformedArgs is the fixture every malformed-path case walks: a secret one
// level down inside a composite, plus siblings that must survive so an
// over-redaction is visible as more than "everything vanished".
func malformedArgs() map[string]any {
	return map[string]any{
		"creds": map[string]any{"token": "tok_abc", "user": "operator"},
		"tags":  []any{"public", "tok_zzz"},
		"other": "public",
	}
}

func TestRedactValueAtPath_MalformedPathFailsClosedWithAnHonestDescriptor(t *testing.T) {
	cases := []struct {
		name string
		path string
		// wantJSON is the whole-args serialization after redaction: the
		// enclosing value collapsed to one token, siblings untouched.
		wantJSON string
		// tokenID is the id the enclosing value's own token is minted under,
		// after its inner leaves are registered.
		tokenID string
		// secrets must not appear anywhere in wantJSON.
		secrets []string
	}{
		{
			name:     "non-integer index mid-path: enclosing map redacted whole, descriptor says so",
			path:     "creds[x].token",
			wantJSON: `{"creds":"<redacted id=\"3\"/>","other":"public","tags":["public","tok_zzz"]}`,
			tokenID:  "3",
			secrets:  []string{"tok_abc"},
		},
		{
			name:     "unterminated bracket mid-path: enclosing map redacted whole, descriptor says so",
			path:     "creds[0",
			wantJSON: `{"creds":"<redacted id=\"3\"/>","other":"public","tags":["public","tok_zzz"]}`,
			tokenID:  "3",
			secrets:  []string{"tok_abc"},
		},
		{
			name:     "non-integer index on an array: enclosing array redacted whole, descriptor says so",
			path:     "tags[x]",
			wantJSON: `{"creds":{"token":"tok_abc","user":"operator"},"other":"public","tags":"<redacted id=\"3\"/>"}`,
			tokenID:  "3",
			secrets:  []string{"tok_zzz"},
		},
		{
			name:     "unterminated bracket on an array: enclosing array redacted whole, descriptor says so",
			path:     "tags[2",
			wantJSON: `{"creds":{"token":"tok_abc","user":"operator"},"other":"public","tags":"<redacted id=\"3\"/>"}`,
			tokenID:  "3",
			secrets:  []string{"tok_zzz"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := malformedArgs()
			r := New()
			err := RedactValueAtPath(m, tc.path, r, tc.path)

			require.Error(t, err, "a malformed declared path must be reported, not swallowed")
			assert.Contains(t, err.Error(), tc.path, "the error must name the path so an operator can grep the spec for it")
			assert.Contains(t, err.Error(), "enclosing", "the error must say what was redacted instead")

			got := mustJSON(t, m)
			assert.Equal(t, tc.wantJSON, got, "the enclosing value must be redacted whole")
			for _, s := range tc.secrets {
				assert.NotContains(t, got, s, "fail-closed: the declared-sensitive value must not survive")
			}

			emitted := r.Emit()
			d, ok := emitted[tc.tokenID]
			require.True(t, ok, "the enclosing value's token must have a descriptor, got %#v", emitted)
			assert.Equal(t, tc.path, d.Name, "the descriptor keeps the declared path so it traces back to the spec")
			assert.Contains(t, d.Description, "malformed",
				"the audit record must admit the declared path never resolved, not claim a clean redaction")
			assert.Contains(t, d.Description, "enclosing",
				"the audit record must say what WAS redacted instead of the declared path")
		})
	}
}

func TestRedactLeafAtPath_MalformedPathFailsClosedWithAnHonestDescriptor(t *testing.T) {
	m := malformedArgs()
	r := New()
	err := RedactLeafAtPath(m, "creds[x].token", r, "creds[x].token")

	require.Error(t, err, "a malformed declared path must be reported, not swallowed")
	assert.Contains(t, err.Error(), "creds[x].token", "the error must name the path")
	assert.Contains(t, err.Error(), "enclosing", "the error must say what was redacted instead")

	got := mustJSON(t, m)
	assert.NotContains(t, got, "tok_abc", "fail-closed: the declared-sensitive value must not survive")
	assert.Equal(t, `{"creds":"<redacted id=\"1\"/>","other":"public","tags":["public","tok_zzz"]}`, got,
		"the enclosing value must be redacted whole")

	emitted := r.Emit()
	d, ok := emitted["1"]
	require.True(t, ok, "the enclosing value's token must have a descriptor, got %#v", emitted)
	assert.Contains(t, d.Description, "malformed",
		"the audit record must admit the declared path never resolved")
}

// TestRedactAtPath_MalformedPathThatResolvesToNothingIsReportedLoudly covers the
// one malformed-path outcome that is NOT fail-closed. When the surviving prefix
// does not resolve either, no redaction happens at all — there is nothing
// addressable left to over-redact — so the error return is the only signal that
// a sensitive-field declaration went unhonored. Silence here is what the
// no-silent-errors rule exists to prevent.
func TestRedactAtPath_MalformedPathThatResolvesToNothingIsReportedLoudly(t *testing.T) {
	cases := []struct {
		name   string
		redact func(map[string]any, string, *Redactor, string) error
	}{
		{name: "RedactValueAtPath: nothing redacted, error names the path", redact: RedactValueAtPath},
		{name: "RedactLeafAtPath: nothing redacted, error names the path", redact: RedactLeafAtPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const path = "nosuchkey[x].token"
			m := malformedArgs()
			before := mustJSON(t, m)
			r := New()

			err := tc.redact(m, path, r, path)

			require.Error(t, err, "an unhonored sensitive-field declaration must never be silent")
			assert.Contains(t, err.Error(), path, "the error must name the path")
			assert.Contains(t, err.Error(), "NO redaction", "the error must say the declaration was not honored at all")
			assert.Equal(t, before, mustJSON(t, m), "nothing resolved, so nothing may be mutated")
			assert.Empty(t, r.Emit(), "nothing resolved, so no descriptor may be emitted")
		})
	}
}
