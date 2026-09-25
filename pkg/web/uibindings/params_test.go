package uibindings_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
)

func TestSubstituteParams(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		params  map[string]string
		want    string
		wantErr bool
	}{
		{
			name:   "top-level placeholder is replaced with the viewer's value",
			args:   `{"since":{"$param":"span"}}`,
			params: map[string]string{"span": "30d"},
			want:   `{"since":"30d"}`,
		},
		{
			name:   "nested and array placeholders are both replaced",
			args:   `{"filter":{"in":[{"$param":"a"},"lit",{"$param":"b"}]}}`,
			params: map[string]string{"a": "x", "b": "y"},
			want:   `{"filter":{"in":["x","lit","y"]}}`,
		},
		{
			name:   "dotted names from a daterange resolve independently",
			args:   `{"from":{"$param":"window.from"},"to":{"$param":"window.to"}}`,
			params: map[string]string{"window.from": "2026-01-01", "window.to": "2026-02-01"},
			want:   `{"from":"2026-01-01","to":"2026-02-01"}`,
		},
		{
			name:   "an object with $param plus another key is ordinary data",
			args:   `{"q":{"$param":"span","extra":1}}`,
			params: map[string]string{"span": "30d"},
			want:   `{"q":{"$param":"span","extra":1}}`,
		},
		{
			name:   "a non-string $param value is ordinary data",
			args:   `{"q":{"$param":7}}`,
			params: map[string]string{},
			want:   `{"q":{"$param":7}}`,
		},
		{
			name:   "empty args stay empty",
			args:   ``,
			params: map[string]string{"span": "30d"},
			want:   ``,
		},
		{
			name:    "a referenced but unsupplied parameter is an error, never an empty string",
			args:    `{"since":{"$param":"span"}}`,
			params:  map[string]string{},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := uibindings.SubstituteParams([]byte(tc.args), tc.params)
			if tc.wantErr {
				require.ErrorIs(t, err, uibindings.ErrUnknownParam)
				return
			}
			require.NoError(t, err)
			if tc.want == "" {
				assert.Empty(t, string(got))
				return
			}
			assert.JSONEq(t, tc.want, string(got))
		})
	}
}

// TestSubstituteParamsRejectsMalformedArgsJSON covers a case the brief's
// table omits: args that fail to parse at all. This must surface as a
// returned error (per AGENTS.md's no-silent-errors rule), never a zero value
// or a panic — the caller's declaration is expected to be well-formed JSON,
// but SubstituteParams must not assume it and must not crash on garbage.
func TestSubstituteParamsRejectsMalformedArgsJSON(t *testing.T) {
	_, err := uibindings.SubstituteParams([]byte(`{"since":`), map[string]string{"span": "30d"})
	require.Error(t, err)
	assert.NotErrorIs(t, err, uibindings.ErrUnknownParam, "a parse failure is a distinct error from an unknown parameter")
}

// TestSubstituteParamsValueIsAlwaysAnEscapedStringLeaf is the adversarial
// case for the structural-safety guarantee: a viewer-controlled param value
// can never change the shape of args, only fill a scalar slot the template
// itself marked substitutable. The value here is itself syntactically valid
// JSON encoding an object — the realistic attack this guarantee must stop is
// a resolver "helpfully" re-parsing a param value and splicing the result
// in, which would let a viewer's string turn a scalar slot into an object
// with sibling keys a downstream resolver reads. The correct result treats
// it as inert text: an ordinary escaped string, never new object keys or a
// replaced container.
func TestSubstituteParamsValueIsAlwaysAnEscapedStringLeaf(t *testing.T) {
	malicious := `{"evil":true,"nested":{"a":1}}`
	got, err := uibindings.SubstituteParams(
		[]byte(`{"q":{"$param":"span"}}`),
		map[string]string{"span": malicious},
	)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Contains(t, decoded, "q")
	require.Len(t, decoded, 1, "the malicious value must not have introduced a sibling key")

	strVal, ok := decoded["q"].(string)
	require.True(t, ok, "the substituted slot must remain a JSON string, never an object/array")
	assert.Equal(t, malicious, strVal)
}

// TestSubstituteParamsIgnoresParamsUnusedByThisTemplate documents that a
// params entry SubstituteParams's own args template never references is
// harmless: it is neither substituted anywhere nor rejected. This function
// only sees one binding's own template, not the declaration's full
// uicomponents.ParamNames vocabulary, so it cannot itself distinguish
// "legitimately belongs to a sibling binding on the same page" from
// "browser invented a name nothing declares" — that check needs the full
// declaration and belongs at the caller that already holds it.
func TestSubstituteParamsIgnoresParamsUnusedByThisTemplate(t *testing.T) {
	got, err := uibindings.SubstituteParams(
		[]byte(`{"since":{"$param":"span"}}`),
		map[string]string{"span": "30d", "region": "unused-by-this-template"},
	)
	require.NoError(t, err)
	assert.JSONEq(t, `{"since":"30d"}`, string(got))
}

// TestSubstituteParamsBoundsRecursionDepth is the adversarial case for
// recursion termination. Args come from the server-side declaration, not the
// browser, but substitute recurses once per nesting level regardless of
// where the template originated, so an unbounded/pathological template is
// still a stack-exhaustion path worth cutting off explicitly rather than
// trusting every future declaration author to keep templates shallow.
//
// The nesting depth used here (200) is deliberately far below
// encoding/json's own decoder depth limit (empirically ~10000-20000) so this
// test exercises SubstituteParams's own bound, not json.Unmarshal's.
func TestSubstituteParamsBoundsRecursionDepth(t *testing.T) {
	const depth = 200
	args := strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth)
	_, err := uibindings.SubstituteParams([]byte(args), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, uibindings.ErrTemplateTooDeep)
}
