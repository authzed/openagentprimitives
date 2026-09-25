package provenance_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
)

// --- Defect 1: StripTags deleted the content it claimed to keep. ------------
//
// The prototype's PtTagRegex has ONE capture group (the id) and its
// replacement is "$2", so every tagged span rendered as the empty string. Its
// doc comment says it preserves the content. Ported verbatim, an agent's
// entire tagged output would vanish on the way to a person.

func TestStripTagsPreservesContent(t *testing.T) {
	got, err := provenance.StripTags(`<pt id="pt_0">hello world</pt>`)
	require.NoError(t, err)
	assert.Equal(t, "hello world", got,
		"stripping markup must leave the data; the prototype's replacement emitted the empty string")
}

// --- Defect 2: multi-line content never matched. ----------------------------
//
// Go's `.` excludes \n without (?s), so ANY tool output containing a newline
// failed to match, ParseTags saw no tags, and the check silently fell back to
// session-wide mode. Tool output is mostly multi-line, so this was the common
// path, not an edge case. Fixed here by scanning rather than by adding a flag.

func TestMultiLineContentIsTagged(t *testing.T) {
	in := "<pt id=\"pt_1\">line one\nline two\nline three</pt>"

	ids, err := provenance.ParseTags(in)
	require.NoError(t, err)
	assert.Equal(t, []provenance.TagID{"pt_1"}, ids,
		"a newline in tool output must not make a tag invisible")

	stripped, err := provenance.StripTags(in)
	require.NoError(t, err)
	assert.Equal(t, "line one\nline two\nline three", stripped)
}

// --- Defect 3: nested tags did not parse. -----------------------------------
//
// A derived tag is REQUIRED to embed its redacted sources verbatim, so nesting
// is the normal shape, not an exotic one. The prototype's non-greedy `.*?`
// terminates at the FIRST </pt>, so the outer tag closed early and the
// remainder of its content was left outside any tag — which then made the
// payload look partially untagged.
//
// Regex cannot parse nesting at all; this is fixed by scanning with a depth
// counter, not by a cleverer pattern.

func TestNestedTagsParseAndTheOuterTagCovers(t *testing.T) {
	in := `<pt id="pt_outer">summary: <pt id="pt_inner">secret</pt> and a tail</pt>`

	ids, err := provenance.ParseTags(in)
	require.NoError(t, err)
	assert.Equal(t, []provenance.TagID{"pt_outer"}, ids,
		"the covering set is the TOP-LEVEL tags; the inner one is the outer tag's provenance, resolved through derived_from")

	stripped, err := provenance.StripTags(in)
	require.NoError(t, err)
	assert.Equal(t, "summary: secret and a tail", stripped,
		"the outer tag must close at ITS OWN close, not the first one — a stranded tail reads as untagged content")

	assert.True(t, provenance.HasOnlyPTags(in),
		"a correctly nested payload is fully covered; terminating early would make this false and silently coarsen the check")
}

// --- Defect 4: union where intersection is required. ------------------------
//
// THE soundness defect. The prototype's ParseTags computed the UNION of
// subjects across tags while tag creation computed the INTERSECTION across
// resources. Concatenating a tag readable by A with one readable by B yielded
// a payload deemed viewable by both — a laundering primitive, and precisely
// the operation an agent performs when assembling an answer from two sources.

func TestCombiningAudiencesIsAnIntersection(t *testing.T) {
	cases := []struct {
		name string
		sets [][]string
		want []string
	}{
		{
			name: "disjoint sources are readable by nobody",
			sets: [][]string{{"user:alice"}, {"user:bob"}},
			want: nil,
		},
		{
			name: "only the shared reader survives",
			sets: [][]string{{"user:alice", "user:carol"}, {"user:alice"}},
			want: []string{"user:alice"},
		},
		{
			name: "a single source is its own audience",
			sets: [][]string{{"user:alice", "user:bob"}},
			want: []string{"user:alice", "user:bob"},
		},
		{
			name: "one empty source empties the result",
			sets: [][]string{{"user:alice"}, {}},
			want: nil,
		},
		{
			name: "no sources at all is readable by nobody, not by everybody",
			sets: nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, provenance.IntersectAudiences(tc.sets),
				"a union here is an unauthorized declassification on every combining operation")
		})
	}
}

// --- The fallback property. -------------------------------------------------
//
// What made the prototype sound was NOT the prompt instructing the model to
// preserve tags — it was hasOnlyPTags: unless the entire payload is
// tag-covered, the check degrades to the conservative session-wide mode. A
// stripped tag must over-block, never go unchecked.

func TestHasOnlyPTagsDecidesCoverage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "fully covered", in: `<pt id="a">x</pt>`, want: true},
		{name: "two tags, fully covered", in: `<pt id="a">x</pt><pt id="b">y</pt>`, want: true},
		{name: "whitespace between tags is not content", in: "<pt id=\"a\">x</pt>\n  <pt id=\"b\">y</pt>", want: true},
		{name: "a trailing untagged tail is NOT covered", in: `<pt id="a">x</pt> and more`, want: false},
		{name: "a leading untagged head is NOT covered", in: `intro <pt id="a">x</pt>`, want: false},
		{name: "entirely untagged is not covered", in: `plain text`, want: false},
		{name: "empty payload is not covered", in: ``, want: false},
		{name: "an unclosed tag is not covered", in: `<pt id="a">x`, want: false},
		{name: "a stray close is not covered", in: `x</pt>`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, provenance.HasOnlyPTags(tc.in),
				"coverage decides per-datum vs session-wide; answering true wrongly is the one failure that goes UNCHECKED rather than over-blocking")
		})
	}
}

// TestMalformedMarkupIsAnErrorNotASilentEmptyParse pins the direction of the
// parse failure.
//
// Returning (nil, nil) for unparseable markup would read to a caller as "this
// payload carries no tags", which is indistinguishable from a genuinely
// untagged payload — and a caller that then skipped the per-datum check would
// have skipped it because parsing broke. An error forces the caller to choose
// the coarse path deliberately.
func TestMalformedMarkupIsAnErrorNotASilentEmptyParse(t *testing.T) {
	for _, in := range []string{`<pt id="a">x`, `x</pt>`, `<pt id="a"><pt id="b">y</pt>`} {
		_, err := provenance.ParseTags(in)
		assert.Error(t, err, "malformed markup %q must be an error, never an empty tag list", in)
	}
}

func TestUntaggedPayloadParsesToNoTagsWithoutError(t *testing.T) {
	ids, err := provenance.ParseTags("plain text")
	require.NoError(t, err, "an untagged payload is well-formed; it simply carries no tags")
	assert.Empty(t, ids)
}
