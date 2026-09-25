package memorycmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func entriesN(n int) []memory.Entry {
	out := make([]memory.Entry, n)
	for i := range out {
		out[i] = memory.Entry{Kind: "label", ID: "label-x", CreatedAt: time.Unix(0, 0).UTC()}
	}
	return out
}

func scoredN(n int) []memory.ScoredEntry {
	out := make([]memory.ScoredEntry, n)
	for i := range out {
		out[i] = memory.ScoredEntry{Entry: memory.Entry{Kind: "label", ID: "label-x"}, Score: 0.5, Source: "pg"}
	}
	return out
}

// TestRenderQueryResultTruncation is the human half of the reported defect:
// `oap memory list` handed back exactly --limit rows and said nothing, so a
// partial listing read as a complete one.
//
// Both directions are pinned. The false-positive one is not decoration: a
// notice printed under every complete listing is noise, and a reader who
// learns to skip it has lost the signal for the listing that really was cut.
func TestRenderQueryResultTruncation(t *testing.T) {
	th := aptest.PlainTheme()

	t.Run("truncated listing names the limit that stopped it and a larger one", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, renderQueryResult(&buf, th, memory.QueryResult{Entries: entriesN(3), Truncated: true}, 3))
		got := buf.String()
		assert.Contains(t, got, "--limit 6", "the remedy must be a flag invocation the reader can copy")
		assert.Contains(t, got, "entries")
		assert.Contains(t, got, "KIND", "the rows are still rendered under the notice")
	})

	t.Run("complete listing says nothing about --limit", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, renderQueryResult(&buf, th, memory.QueryResult{Entries: entriesN(3)}, 100))
		assert.NotContains(t, buf.String(), "--limit", "a complete listing must not carry a truncation notice")
	})

	t.Run("dropped predicates and truncation are separate facts and both are reported", func(t *testing.T) {
		var buf bytes.Buffer
		res := memory.QueryResult{
			Entries:           entriesN(2),
			DroppedPredicates: []string{"FieldEquals"},
			Partial:           true,
			Truncated:         true,
		}
		require.NoError(t, renderQueryResult(&buf, th, res, 2))
		got := buf.String()
		assert.Contains(t, got, "FieldEquals", "the dropped-predicate warning must survive")
		assert.Contains(t, got, "--limit 4", "and so must the truncation notice")
	})
}

// TestRenderSearchResultTruncation pins the same pair for the ranked read.
func TestRenderSearchResultTruncation(t *testing.T) {
	th := aptest.PlainTheme()

	t.Run("truncated ranking says so and names a larger --limit", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, renderSearchResult(&buf, th, memory.MergedSearchResult{Entries: scoredN(2), Truncated: true}, 2))
		got := buf.String()
		assert.Contains(t, got, "--limit 4")
		assert.Contains(t, got, "results")
	})

	t.Run("complete ranking says nothing about --limit", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, renderSearchResult(&buf, th, memory.MergedSearchResult{Entries: scoredN(2)}, 20))
		assert.NotContains(t, buf.String(), "--limit")
	})
}

// TestKindCountTruncation covers the sharpest form of the defect on this
// surface: `oap memory kinds` prints a COUNT, and a count capped by a query
// limit is not a smaller count — it is a wrong one, presented as a fact.
func TestKindCountTruncation(t *testing.T) {
	cases := []struct {
		name string
		sum  kindSummary
		want string
	}{
		{name: "a capped count is marked, never printed as exact", sum: kindSummary{Count: 10000, Truncated: true}, want: "10000+"},
		{name: "an exact count is printed bare", sum: kindSummary{Count: 42}, want: "42"},
		{name: "zero is exact", sum: kindSummary{Count: 0}, want: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatKindCount(tc.sum))
		})
	}
}

// TestKindSummaryJSONCarriesTruncation pins the machine-readable half of the
// count: a script reading --json must be able to tell a capped count from an
// exact one, which the rendered "+" does not travel as.
func TestKindSummaryJSONCarriesTruncation(t *testing.T) {
	b, err := json.Marshal(kindSummary{Name: "turn", Count: 10000, Truncated: true})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"truncated":true`)

	b, err = json.Marshal(kindSummary{Name: "turn", Count: 3})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"truncated":false`,
		"the field must be present on an exact count too, or absence stays ambiguous")
}

// TestQueryResultJSONShapeIsWhatTheCommandEmits documents, at the surface a
// script actually reads, that --json carries the truncation fact: the value
// `oap memory list --json` encodes IS the memory.QueryResult.
func TestQueryResultJSONShapeIsWhatTheCommandEmits(t *testing.T) {
	b, err := json.Marshal(memory.QueryResult{Entries: entriesN(1), Truncated: true})
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(b), `"Truncated":true`),
		"the --json document must state the truncation, not only the table")
}
