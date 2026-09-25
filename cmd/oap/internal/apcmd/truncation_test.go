package apcmd

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTruncationNotice pins the two things the notice owes its reader: which
// limit ended the listing, and a concrete larger one that will not. A notice
// that says only "results may be incomplete" leaves the reader doing what the
// reporter of this defect did — guessing a bigger number and re-running.
func TestTruncationNotice(t *testing.T) {
	cases := []struct {
		name  string
		noun  string
		limit int
		// wantRemedy is the exact flag invocation the message must hand back.
		wantRemedy string
	}{
		{name: "the entry-listing default: names --limit 100 and offers 200", noun: "entries", limit: 100, wantRemedy: "--limit 200"},
		{name: "the ranked default: names --limit 20 and offers 40", noun: "results", limit: 20, wantRemedy: "--limit 40"},
		{name: "limit 1 offers 2", noun: "entries", limit: 1, wantRemedy: "--limit 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, TruncationNotice(&buf, tc.noun, tc.limit))
			got := buf.String()

			assert.Contains(t, got, strconv.Itoa(tc.limit), "the notice must name the limit that stopped the listing")
			assert.Contains(t, got, tc.noun, "the notice must name what was counted")
			assert.Contains(t, got, tc.wantRemedy, "the remedy must be a flag the reader can copy")
			assert.True(t, strings.HasSuffix(got, "\n"), "the notice must end its own line")
		})
	}
}

// TestTruncationNoticeNeverOffersAnUnusableLimit guards the arithmetic: the
// suggested limit is derived from the one that truncated, and a suggestion that
// overflowed to a negative number is worse than no suggestion — cobra refuses
// it, so the remedy the message names would not work.
func TestTruncationNoticeNeverOffersAnUnusableLimit(t *testing.T) {
	for _, limit := range []int{math.MaxInt, math.MaxInt - 1, math.MaxInt/2 + 1} {
		var buf bytes.Buffer
		require.NoError(t, TruncationNotice(&buf, "entries", limit))
		assert.NotRegexp(t, `--limit\s+-`, buf.String(), "a suggestion that overflowed to a negative limit is a remedy cobra refuses")
		assert.Contains(t, buf.String(), "--limit", "the notice still has to name the flag to raise")
	}
}
