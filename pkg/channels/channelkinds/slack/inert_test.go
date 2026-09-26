package slack

// Direct unit tests for the escaper/sweep helpers in inert.go — the ones that
// exercise a helper by name against its own output. The behavioral coverage
// that drives these helpers through a render surface (the App Home view, the
// interaction card, the metaagent card, the details modal, the join notice)
// lives with each surface, in the *_inertness_test.go / *_inert_test.go files.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEscapeSlackText pins the escape set AND the single-pass property that
// makes it order-independent: a strings.Replacer never re-scans its own
// output, so "&lt;" becomes "&amp;lt;" rather than "&amp;amp;lt;". A
// hand-rolled ReplaceAll chain only behaves this way when "&" happens to run
// first, which is why this package has exactly one escaper.
//
// It also pins what is deliberately NOT escaped: publishers compose backticks,
// `*` and `_` on purpose (toolApprovalFields backtick-wraps the tool name),
// and escaping those would mangle real cards for no security gain.
func TestEscapeSlackText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"link syntax: both angle brackets escaped", "<a|b>", "&lt;a|b&gt;"},
		{"channel-wide ping cannot survive", "<!channel>", "&lt;!channel&gt;"},
		{"bare ampersand escaped once", "tom & jerry", "tom &amp; jerry"},
		{"single pass: a pre-existing entity is escaped, never re-scanned", "&lt;", "&amp;lt;"},
		{"deliberate publisher markup is left live", "Tool `git_push` is _required_ and *bold*", "Tool `git_push` is _required_ and *bold*"},
		{"plain text untouched", "401 Unauthorized -- Bad credentials", "401 Unauthorized -- Bad credentials"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, escapeSlackText(tc.in))
		})
	}
}

// TestEscapeSlackTexts pins the slice wrapper's three obligations the scalar
// escapeSlackText cannot speak to: it escapes PER ENTRY (so a caller composing
// its own markup around each one — the join notice's participant list — is not
// escaped along with them), it returns without allocating on an empty input,
// and it leaves the caller's input slice untouched.
func TestEscapeSlackTexts(t *testing.T) {
	t.Run("empty input: returns empty for both nil and a zero-length slice", func(t *testing.T) {
		assert.Empty(t, escapeSlackTexts(nil), "nil in, nothing out")
		assert.Empty(t, escapeSlackTexts([]string{}), "empty in, empty out")
	})

	t.Run("each entry is escaped independently", func(t *testing.T) {
		in := []string{"<a|b>", "tom & jerry", "<!channel>", "plain text"}
		want := []string{"&lt;a|b&gt;", "tom &amp; jerry", "&lt;!channel&gt;", "plain text"}
		assert.Equal(t, want, escapeSlackTexts(in),
			"the escape must land on every entry before a caller wraps its own markup around each")
	})

	t.Run("returns a new slice and does not mutate the caller's input", func(t *testing.T) {
		in := []string{"<a|b>", "x & y"}
		orig := append([]string(nil), in...)
		got := escapeSlackTexts(in)

		require.Equal(t, orig, in, "the input slice must be left exactly as the caller passed it")
		assert.Equal(t, []string{"&lt;a|b&gt;", "x &amp; y"}, got,
			"and the returned slice carries the escaped values")
	})
}

// TestInertExcerptUsesTheSharedEscaper: the excerpt path adds fence-breaking
// protection on top of the shared escaper, and must not have drifted into a
// second implementation of the escape itself.
func TestInertExcerptUsesTheSharedEscaper(t *testing.T) {
	assert.Equal(t, escapeSlackText("<a|b>"), inertExcerpt("<a|b>"),
		"with no code fence to break, inertExcerpt must equal the shared escaper exactly")
	assert.NotContains(t, inertExcerpt("```\n<!here>"), "```",
		"...while still neutralizing fence breakers, which is the only thing it adds")
}

// TestInertExcerpt_Characterization pins the security-critical inert treatment
// of the untrusted excerpt before it is placed inside a code fence: backtick
// triplets neutralized (so the excerpt cannot break the surrounding fence) and
// & < > HTML-escaped (so <!channel>/<!here> pings and tags never fire). The
// renderer-level invariant it feeds — headline plus code-fenced inert excerpt —
// is pinned separately by TestBuildInteractionRequestBlocks_RendersInertExcerpt
// in interaction_excerpt_test.go.
func TestInertExcerpt_Characterization(t *testing.T) {
	require.Equal(t, "ʼʼʼnot a fence", inertExcerpt("```not a fence"),
		"backtick triplets must be neutralized to modifier apostrophes")
	require.Equal(t, "&lt;!channel&gt; &amp; &lt;tag&gt;", inertExcerpt("<!channel> & <tag>"),
		"& < > must be HTML-escaped")
}

// TestCapInertRunesIsIdempotent pins the property this package has now shipped
// two escapers without: neither half of inertProse survives a second pass, and
// a REPAIR that does not survive one is the same bug in a new place. Re-running
// the cap over its own output is the shape a future renderer reaches for when it
// caps a composed line whose parts were already capped.
func TestCapInertRunesIsIdempotent(t *testing.T) {
	cases := []struct {
		name  string
		swept string
	}{
		{
			name:  "a cut inside the span the sweep added",
			swept: inertProse("see " + bareLureURL + "/" + strings.Repeat("a", 400)),
		},
		{
			name:  "a cut inside a fence the sweep skipped",
			swept: inertProse(fenceDelimiter + "\nsee " + bareLureURL + "/" + strings.Repeat("a", 400) + "\n" + fenceDelimiter),
		},
		{
			// What the first-line strip hands the cap: short enough that no
			// truncation happens at all, so the repair is the only thing acting.
			name: "a fence the strip orphaned before the cap ever ran",
			swept: strings.SplitN(
				inertProse("lede "+fenceDelimiter+bareLureURL+"\nrest"+fenceDelimiter), "\n", 2)[0],
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+": re-capping its own output changes nothing", func(t *testing.T) {
			const budget = 200
			once := capInertRunes(tc.swept, budget)
			assert.Equal(t, once, capInertRunes(once, budget),
				"a second pass must not undo the first — that is how defuseBareLinks re-livens a URL")
			assert.Empty(t, liveLinkIn(once), "and the once-capped string is inert to begin with")
			assert.LessOrEqual(t, len([]rune(once)), budget, "every repair is paid out of the budget")
		})
	}
}

// TestCapInertRunes_MeasuresParityOnTheTailRegionOnly is the cap's contract
// stated at the DELIMITER level, one layer below the rendering rule liveLinkIn
// models — it says the cap leaves no pair half-written, whatever that pair
// would have rendered as.
//
// A code FENCE contributes three backticks, so whole-string SPAN parity flips
// on it: an unterminated fence plus one span the cut left hanging reads EVEN —
// "nothing to repair" — while the span is still open. Only the region after the
// last fence delimiter, where defuseUnfenced's pairs actually live, answers the
// question the span check is asking, and it only answers it once the fence
// check has run. The requester types the Verbatim on the metaagent card, so an
// unbalanced fence is theirs to choose.
func TestCapInertRunes_MeasuresParityOnTheTailRegionOnly(t *testing.T) {
	run := "see " + bareLureURL + "/" + strings.Repeat("a", 400)

	cases := []struct {
		name  string
		swept string
	}{
		{
			name:  "no fence: the cut inside a span is re-closed",
			swept: inertProse(run),
		},
		{
			name:  "balanced fence ahead of the run: still re-closed",
			swept: inertProse(fenceDelimiter + "\nx\n" + fenceDelimiter + "\n" + run),
		},
		{
			// The sweep DID sweep this tail (an odd delimiter count fails
			// closed), so the cut lands inside a span it added — and the fence
			// it sits under is closed on the way out, which is what makes the
			// whole-string parity that would have read "nothing to repair"
			// stop mattering.
			name:  "unterminated fence ahead of the run: both delimiters closed, where whole-string parity would close neither",
			swept: inertProse(fenceDelimiter + "\n" + run),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const budget = 200
			capped := capInertRunes(tc.swept, budget)

			require.NotEqual(t, tc.swept, capped, "the fixture must actually be truncated")
			assert.LessOrEqual(t, len([]rune(capped)), budget,
				"re-closing a delimiter is paid out of the budget, not added to it")

			assert.Zero(t, strings.Count(capped, fenceDelimiter)%2,
				"a fence the cut orphaned must be closed, or the region it opened renders unswept")
			parts := strings.Split(capped, fenceDelimiter)
			assert.Zero(t, strings.Count(parts[len(parts)-1], "`")%2,
				"the region where the sweep's spans live must end with every one of them closed")
		})
	}
}
