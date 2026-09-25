package uiselect_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uiselect"
)

// testMaxFoundKeys mirrors uiselect's unexported maxFoundKeys. It is a local
// constant, not an import, because the cap is deliberately not part of the
// package's public API — only the resulting behavior (Found is capped and
// sorted) is.
const testMaxFoundKeys = 24

func TestParseAcceptsTheGrammar(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []uiselect.Segment
	}{
		{name: "a single plain key", in: "total", want: []uiselect.Segment{{Key: "total"}}},
		{name: "a single iterating key", in: "results[]", want: []uiselect.Segment{{Key: "results", Iterate: true}}},
		{name: "an iterating key followed by a plain key", in: "results[].properties", want: []uiselect.Segment{
			{Key: "results", Iterate: true}, {Key: "properties"},
		}},
		{name: "three plain keys", in: "a.b.c", want: []uiselect.Segment{{Key: "a"}, {Key: "b"}, {Key: "c"}}},
		{name: "two iterating keys", in: "a[].b[]", want: []uiselect.Segment{
			{Key: "a", Iterate: true}, {Key: "b", Iterate: true},
		}},
		{name: "the empty string is legal and selects the whole value", in: "", want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := uiselect.Parse(tc.in)
			require.NoError(t, err)
			if tc.in == "" {
				assert.True(t, sel.Empty())
			} else {
				assert.False(t, sel.Empty())
			}
			assert.Equal(t, tc.want, sel.Segments())
			assert.Equal(t, tc.in, sel.String(), "String() must round-trip the input exactly")
		})
	}
}

func TestParseRejects(t *testing.T) {
	// overLong and tooManySegments are FIXED magic numbers (300, 17), built
	// programmatically via strings.Repeat rather than a transcribed literal —
	// hand-typing 300 characters risks silent drift on a later copy-paste
	// edit. They are deliberately NOT derived from MaxSelectorLen/MaxSegments:
	// a derived count would auto-adjust if either constant were ever raised,
	// which would hide exactly the mutation this file's bound tests exist to
	// catch (the constant changing without Parse's enforcement changing too).
	overLong := strings.Repeat("a", 300)
	tooManySegments := strings.Repeat("a.", 16) + "a" // 17 segments

	cases := []struct {
		name    string
		in      string
		wantErr error
	}{
		{name: "a lone dot", in: ".", wantErr: uiselect.ErrSyntax},
		{name: "a leading dot", in: ".a", wantErr: uiselect.ErrSyntax},
		{name: "a trailing dot", in: "a.", wantErr: uiselect.ErrSyntax},
		{name: "a doubled dot", in: "a..b", wantErr: uiselect.ErrSyntax},
		{name: "an unterminated bracket", in: "a[", wantErr: uiselect.ErrSyntax},
		{name: "a stray close bracket", in: "a]", wantErr: uiselect.ErrSyntax},
		{name: "an index", in: "a[0]", wantErr: uiselect.ErrSyntax},
		{name: "a wildcard", in: "a[*]", wantErr: uiselect.ErrSyntax},
		{name: "a filter expression", in: "a[?(@.x)]", wantErr: uiselect.ErrSyntax},
		{name: "trailing garbage after []", in: "a[]b", wantErr: uiselect.ErrSyntax},
		{name: "brackets with no key", in: "[]", wantErr: uiselect.ErrSyntax},
		{name: "over MaxSelectorLen", in: overLong, wantErr: uiselect.ErrTooLong},
		{name: "over MaxSegments", in: tooManySegments, wantErr: uiselect.ErrTooManySegments},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := uiselect.Parse(tc.in)
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr), "got %v, want it to wrap %v", err, tc.wantErr)
		})
	}
}

func compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Compact(&buf, b))
	return buf.Bytes()
}

const envelope = `{"results":[{"id":"1","properties":{"name":"Acme"}},{"id":"2","properties":{"name":"Globex"}}],"total":7,"paging":{"next":"abc"}}`

func TestApplyExtracts(t *testing.T) {
	cases := []struct {
		name          string
		sel           string
		doc           string
		want          string
		byteIdentical bool
	}{
		{name: "empty selector returns the whole value, byte-identical", sel: "", doc: envelope, want: envelope, byteIdentical: true},
		{name: "a scalar field", sel: "total", doc: envelope, want: `7`},
		{name: "an iterated array", sel: "results[]", doc: envelope,
			want: `[{"id":"1","properties":{"name":"Acme"}},{"id":"2","properties":{"name":"Globex"}}]`},
		{name: "the motivating case: an array of objects reduced to one nested field", sel: "results[].properties", doc: envelope,
			want: `[{"name":"Acme"},{"name":"Globex"}]`},
		{name: "nested iteration does not flatten", sel: "a[].b[]", doc: `{"a":[{"b":[1,2]},{"b":[3]}]}`,
			want: `[[1,2],[3]]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := uiselect.Parse(tc.sel)
			require.NoError(t, err)
			got, err := sel.Apply(json.RawMessage(tc.doc))
			require.NoError(t, err)
			if tc.byteIdentical {
				// The un-round-tripped guarantee is the claim under test here:
				// assert.Equal on the raw bytes, not assert.JSONEq, which would
				// accept a re-encoding that merely represents the same value.
				assert.Equal(t, tc.doc, string(got))
				return
			}
			assert.JSONEq(t, tc.want, string(got))
		})
	}
}

// TestApplyDistinguishesAMissFromAnEmptyResult pins the rule most likely to be
// got wrong: an absent key and a present-but-empty value are different facts,
// and conflating them in either direction is the defect this package exists
// to prevent.
func TestApplyDistinguishesAMissFromAnEmptyResult(t *testing.T) {
	cases := []struct {
		name        string
		sel         string
		doc         string
		wantErrIs   error // nil means "no error"
		wantValue   string
		wantKind    string
		wantFound   []string
		wantElement int // -1 when not asserted / not under iteration
	}{
		{
			name:      "an iterated key present but holding an empty array succeeds",
			sel:       "results[]",
			doc:       `{"results":[]}`,
			wantValue: `[]`,
		},
		{
			name:      "a plain key present but holding an empty object succeeds",
			sel:       "results",
			doc:       `{"results":{}}`,
			wantValue: `{}`,
		},
		{
			name:      "a key genuinely absent from the object is a miss, not an empty result",
			sel:       "results",
			doc:       `{"items":[]}`,
			wantErrIs: uiselect.ErrNoMatch,
			wantFound: []string{"items"},
		},
		{
			name:      "an iterated key whose value is not an array",
			sel:       "results[]",
			doc:       `{"results":{"a":1}}`,
			wantErrIs: uiselect.ErrNotArray,
			wantKind:  "object",
		},
		{
			name:      "descending a key on null",
			sel:       "a.b",
			doc:       `{"a":null}`,
			wantErrIs: uiselect.ErrNotObject,
			wantKind:  "null",
		},
		{
			name:      "a terminal null is a match and is returned",
			sel:       "a",
			doc:       `{"a":null}`,
			wantValue: `null`,
		},
		{
			name:        "one element under iteration fails: the WHOLE selector fails, naming that element",
			sel:         "results[].properties",
			doc:         `{"results":[{"properties":{}},{"id":"2"}]}`,
			wantErrIs:   uiselect.ErrNoMatch,
			wantElement: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := uiselect.Parse(tc.sel)
			require.NoError(t, err)
			got, err := sel.Apply(json.RawMessage(tc.doc))

			if tc.wantErrIs == nil {
				require.NoError(t, err)
				assert.JSONEq(t, tc.wantValue, string(got))
				return
			}

			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErrIs), "got %v, want it to wrap %v", err, tc.wantErrIs)

			var me *uiselect.MissError
			require.ErrorAs(t, err, &me)
			if tc.wantKind != "" {
				assert.Equal(t, tc.wantKind, me.Kind)
			}
			if tc.wantFound != nil {
				assert.Equal(t, tc.wantFound, me.Found)
			}
			if tc.name == "one element under iteration fails: the WHOLE selector fails, naming that element" {
				// Asserted explicitly, not folded into "an error occurred": an
				// implementation that DROPS the bad element instead of failing
				// would return one row and no error at all, and one that fails on
				// the wrong element would still satisfy a bare error check.
				assert.Equal(t, tc.wantElement, me.Element)
			}
		})
	}
}

// TestMissErrorNamesKeysAndNeverValues asserts the diagnostic contract: a
// MissError's text carries the selector, the failing segment, and the KEYS
// present at that point — never any VALUE from the document, because Found is
// the operator-facing diagnostic and this repo's user-facing-text rule keeps
// upstream data out of any surface derived from it.
func TestMissErrorNamesKeysAndNeverValues(t *testing.T) {
	doc := `{"items":["leaked-value-marker"],"other":"also-leaked-value-marker"}`
	sel, err := uiselect.Parse("results")
	require.NoError(t, err)

	_, err = sel.Apply(json.RawMessage(doc))
	require.Error(t, err)
	var me *uiselect.MissError
	require.ErrorAs(t, err, &me)

	msg := me.Error()
	assert.Contains(t, msg, "results")
	for _, k := range me.Found {
		assert.Contains(t, msg, k)
	}
	assert.NotContains(t, msg, "leaked-value-marker")
	assert.NotContains(t, msg, "also-leaked-value-marker")
}

func TestMissErrorCapsFoundKeys(t *testing.T) {
	obj := map[string]any{}
	for i := 0; i < testMaxFoundKeys+10; i++ {
		obj[strings.Repeat("k", i+1)] = i
	}
	doc, err := json.Marshal(map[string]any{"container": obj})
	require.NoError(t, err)

	sel, err := uiselect.Parse("container.missing")
	require.NoError(t, err)
	_, err = sel.Apply(doc)
	require.Error(t, err)

	var me *uiselect.MissError
	require.ErrorAs(t, err, &me)
	require.Len(t, me.Found, testMaxFoundKeys)
	assert.True(t, sort.StringsAreSorted(me.Found), "Found must be sorted so the message is deterministic")
}

// TestApplyNeverGrowsTheDocument is the transitive-ceiling property: since
// the UI ingress ceiling stays on the RAW upstream response, extraction must
// never be able to produce a document larger than the (compacted) input it
// was applied to. Drawn from the success rows of TestApplyExtracts and
// TestApplyDistinguishesAMissFromAnEmptyResult.
func TestApplyNeverGrowsTheDocument(t *testing.T) {
	cases := []struct {
		name string
		sel  string
		doc  string
	}{
		{name: "empty selector", sel: "", doc: envelope},
		{name: "scalar field", sel: "total", doc: envelope},
		{name: "iterated array", sel: "results[]", doc: envelope},
		{name: "nested field via iteration", sel: "results[].properties", doc: envelope},
		{name: "nested iteration", sel: "a[].b[]", doc: `{"a":[{"b":[1,2]},{"b":[3]}]}`},
		{name: "iterated empty array", sel: "results[]", doc: `{"results":[]}`},
		{name: "plain empty object", sel: "results", doc: `{"results":{}}`},
		{name: "terminal null", sel: "a", doc: `{"a":null}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := uiselect.Parse(tc.sel)
			require.NoError(t, err)
			got, err := sel.Apply(json.RawMessage(tc.doc))
			require.NoError(t, err)
			assert.LessOrEqual(t, len(got), len(compact(t, []byte(tc.doc))))
		})
	}
}

// TestApplyCanGrowRelativeToCompactInputViaJSONEscaping is NOT a required
// task test — it documents a counterexample to the "extraction can only
// shrink" property found while satisfying TestApplyNeverGrowsTheDocument's
// corpus above. encoding/json's default marshaler escapes '<', '>', '&' to
// six-byte "\u00XX" sequences (HTML-embedding safety) and ALSO escapes
// U+2028/U+2029 unconditionally (JavaScript string-literal safety) with no
// way to turn either off short of hand-rolling a marshaler. Extracting a
// single string field that is disproportionately made of such characters,
// out of a document whose only other content is the wrapping object, can
// therefore re-encode LARGER than the compacted whole document: dropping the
// `{"a":...}` wrapper saves 4 bytes, but escaping ten '<' costs 50. This
// package still only ever SUBSETS the decoded value (it never adds a key or
// a byte of upstream data), so the growth is confined to string re-encoding
// overhead, not to the traversal logic — but the byte-count property some
// downstream reasoning might assume (e.g. sizing an ingress ceiling off of
// it) does not hold in general and must not be asserted as if it did.
func TestApplyCanGrowRelativeToCompactInputViaJSONEscaping(t *testing.T) {
	doc := `{"a":"<<<<<<<<<<"}`
	sel, err := uiselect.Parse("a")
	require.NoError(t, err)

	got, err := sel.Apply(json.RawMessage(doc))
	require.NoError(t, err)

	compacted := compact(t, []byte(doc))
	assert.Greater(t, len(got), len(compacted),
		"got %d bytes (%s) vs compacted input %d bytes (%s): extraction grew the document",
		len(got), got, len(compacted), compacted)
}

// TestApplyPreservesLargeIntegerPrecision is the UseNumber test. Without it
// the code still compiles and every other test still passes, and a large
// integer ID silently becomes a float64 that re-encodes with lost digits —
// exactly the field (an object ID) a table is most likely to select.
func TestApplyPreservesLargeIntegerPrecision(t *testing.T) {
	doc := `{"results":[{"id":12345678901234567890},{"id":98765432109876543210}]}`
	sel, err := uiselect.Parse("results[].id")
	require.NoError(t, err)

	got, err := sel.Apply(json.RawMessage(doc))
	require.NoError(t, err)
	assert.Contains(t, string(got), "12345678901234567890")
	assert.Contains(t, string(got), "98765432109876543210")
}

func TestApplyRejectsANonJSONValue(t *testing.T) {
	sel, err := uiselect.Parse("total")
	require.NoError(t, err)

	_, err = sel.Apply(json.RawMessage("not json"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, uiselect.ErrValueNotJSON))

	// A malformed upstream value is not the author's selector being wrong;
	// reporting it as a MissError would send an author hunting a selector
	// that was never the problem.
	var me *uiselect.MissError
	assert.False(t, errors.As(err, &me))
}
