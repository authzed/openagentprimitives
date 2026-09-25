package sqlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	searchsqlite "github.com/authzed/openagentprimitives/pkg/memory/search/sqlite"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
)

func newProvider(t *testing.T) (*searchsqlite.SearchProvider, *memsqlite.Backend, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	c, err := memsqlite.NewClient(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Migrate(context.Background()))
	p, err := searchsqlite.New(c)
	require.NoError(t, err, "FTS migrate")
	return p, memsqlite.NewBackend(c), c.DB()
}

// countFTSRows returns the number of memory_fts rows matching the given filters.
func countFTSRows(t *testing.T, db *sql.DB, scope memory.Scope, kind, entryID string) int {
	t.Helper()
	var count int
	row := db.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM memory_fts WHERE scope_kind=? AND scope_id=? AND kind=? AND entry_id=?",
		scope.Kind, scope.ID, kind, entryID,
	)
	require.NoError(t, row.Scan(&count))
	return count
}

// countFTSRowsForScope returns the number of memory_fts rows matching the given scope.
func countFTSRowsForScope(t *testing.T, db *sql.DB, scope memory.Scope) int {
	t.Helper()
	var count int
	row := db.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM memory_fts WHERE scope_kind=? AND scope_id=?",
		scope.Kind, scope.ID,
	)
	require.NoError(t, row.Scan(&count))
	return count
}

func TestProvider_Capabilities(t *testing.T) {
	p, _, _ := newProvider(t)
	caps := p.SearchCapabilities()
	assert.True(t, caps.TextSearch)
	// FieldFilters is false: Search does not evaluate req.Fields content
	// predicates (see TestProvider_SearchFieldsAreDropped).
	assert.False(t, caps.FieldFilters)
	assert.True(t, caps.TagFilters)
	assert.True(t, caps.TimeRange)
	assert.False(t, caps.VectorSearch)
	assert.Equal(t, "sqlite", p.Name())
}

func TestProvider_IndexThenDelete(t *testing.T) {
	p, _, db := newProvider(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	// After indexing key "a-1" once, exactly 1 row for that key.
	require.NoError(t, p.Index(ctx, scope, "alpha", "a-1", []byte(`{"text":"hello world"}`)))
	assert.Equal(t, 1, countFTSRows(t, db, scope, "alpha", "a-1"), "should have 1 row for a-1 after Index")

	// Re-index same key (delete-then-insert) must not duplicate.
	require.NoError(t, p.Index(ctx, scope, "alpha", "a-1", []byte(`{"text":"hello world again"}`)))
	assert.Equal(t, 1, countFTSRows(t, db, scope, "alpha", "a-1"), "should still have 1 row for a-1 after re-Index")

	// Index a second key under same scope.
	require.NoError(t, p.Index(ctx, scope, "alpha", "a-2", []byte(`{"text":"goodbye"}`)))
	assert.Equal(t, 1, countFTSRows(t, db, scope, "alpha", "a-2"), "should have 1 row for a-2 after Index")
	assert.Equal(t, 2, countFTSRowsForScope(t, db, scope), "should have 2 rows total for scope")

	// After DeleteIndex("a-1"), 0 rows for "a-1" but "a-2" remains.
	require.NoError(t, p.DeleteIndex(ctx, scope, "alpha", "a-1"))
	assert.Equal(t, 0, countFTSRows(t, db, scope, "alpha", "a-1"), "should have 0 rows for a-1 after DeleteIndex")
	assert.Equal(t, 1, countFTSRows(t, db, scope, "alpha", "a-2"), "should still have 1 row for a-2")
	assert.Equal(t, 1, countFTSRowsForScope(t, db, scope), "should have 1 row total for scope")

	// After DeleteScopeIndex, all rows for the scope are gone.
	require.NoError(t, p.DeleteScopeIndex(ctx, scope))
	assert.Equal(t, 0, countFTSRowsForScope(t, db, scope), "should have 0 rows for scope after DeleteScopeIndex")
}

// putIndexed writes an entry through both the storage backend (memory_entry,
// what Search's JOIN reconstructs full Entries from) and the search
// provider's FTS index (memory_fts, what MATCH runs over). The brief's
// Search implementation joins the two tables, so a row must exist in both
// for Search to return it — Index alone (as in the task brief's original
// test snippets) is not enough.
func putIndexed(t *testing.T, p *searchsqlite.SearchProvider, backend *memsqlite.Backend, scope memory.Scope, kind, id, text string) {
	t.Helper()
	ctx := context.Background()
	content, err := json.Marshal(text)
	require.NoError(t, err)
	require.NoError(t, backend.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      kind,
		ID:        id,
		CreatedAt: time.Now(),
		Content:   content,
	}))
	require.NoError(t, p.Index(ctx, scope, kind, id, []byte(text)))
}

// putIndexedFull is putIndexed's more configurable sibling: it accepts
// explicit tags and createdAt so tests can exercise the structured
// filters (Tags, Since/Until) that Search now honors as extra WHERE
// clauses on the joined memory_entry row.
func putIndexedFull(t *testing.T, p *searchsqlite.SearchProvider, backend *memsqlite.Backend, scope memory.Scope, kind, id, text string, tags []string, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	content, err := json.Marshal(text)
	require.NoError(t, err)
	require.NoError(t, backend.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      kind,
		ID:        id,
		CreatedAt: createdAt,
		Tags:      tags,
		Content:   content,
	}))
	require.NoError(t, p.Index(ctx, scope, kind, id, []byte(text)))
}

func TestProvider_SearchRanksMatches(t *testing.T) {
	p, backend, _ := newProvider(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	putIndexed(t, p, backend, scope, "alpha", "a-1", "the quick brown fox")
	putIndexed(t, p, backend, scope, "alpha", "a-2", "a slow green turtle")

	res, err := p.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{scope}, Text: "fox"})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-1", res.Entries[0].Entry.ID)
	assert.Equal(t, "sqlite", res.Entries[0].Source)
	assert.Greater(t, res.Entries[0].Score, 0.0, "score normalized to higher=better")
}

func TestProvider_SearchScopeIsolation(t *testing.T) {
	p, backend, _ := newProvider(t)
	ctx := context.Background()
	scopeA := memory.Scope{Kind: "session", ID: "ns/a"}
	scopeB := memory.Scope{Kind: "session", ID: "ns/b"}
	putIndexed(t, p, backend, scopeA, "alpha", "a-1", "shared keyword")
	putIndexed(t, p, backend, scopeB, "alpha", "b-1", "shared keyword")

	res, err := p.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{scopeA}, Text: "keyword"})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-1", res.Entries[0].Entry.ID)
}

func TestProvider_SearchEmptyTextReturnsNothing(t *testing.T) {
	p, backend, _ := newProvider(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	putIndexed(t, p, backend, scope, "alpha", "a-1", "anything")
	res, err := p.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{scope}, Text: ""})
	require.NoError(t, err)
	assert.Empty(t, res.Entries)
}

// TestProvider_SearchEscapesSpecialCharacters covers Finding 1: raw
// req.Text fed straight into `memory_fts MATCH ?` lets FTS5's own query
// grammar reinterpret ordinary natural-language text (apostrophes,
// colons, leading hyphens, bare keywords) as operators and error out.
// Each case seeds one matching row and asserts Search finds it without
// error, proving the text round-trips as a literal search term.
func TestProvider_SearchEscapesSpecialCharacters(t *testing.T) {
	cases := []struct {
		name    string
		indexed string
		query   string
	}{
		{name: "apostrophe: matches despite contraction", indexed: "it isn't broken", query: "isn't"},
		{name: "colon: matches despite FTS5 column-filter syntax", indexed: "ratio is foo:bar today", query: "foo:bar"},
		{name: "leading hyphen: matches despite FTS5 NOT-prefix syntax", indexed: "value is -x here", query: "-x"},
		{name: "bare keyword: matches despite FTS5 boolean operator", indexed: "the AND gate failed", query: "AND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, backend, _ := newProvider(t)
			ctx := context.Background()
			scope := memory.Scope{Kind: "session", ID: "ns/a"}
			putIndexed(t, p, backend, scope, "alpha", "a-1", tc.indexed)

			res, err := p.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{scope}, Text: tc.query})
			require.NoError(t, err, "Search must not error on FTS5-grammar-sensitive text")
			require.Len(t, res.Entries, 1)
			assert.Equal(t, "a-1", res.Entries[0].Entry.ID)
		})
	}
}

func TestProvider_SearchAllPunctuationTextReturnsNothing(t *testing.T) {
	p, backend, _ := newProvider(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	putIndexed(t, p, backend, scope, "alpha", "a-1", "anything")

	res, err := p.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{scope}, Text: "   -- ::  "})
	require.NoError(t, err, "text that tokenizes to zero terms must not error")
	assert.Empty(t, res.Entries)
}

// TestProvider_SearchTagsFilter covers Finding 2: two entries share the
// same search term but carry different tags. Without honoring
// req.Tags, both would match and CompositeSearcher (no post-filtering)
// would leak the wrong one past a caller's tag filter.
func TestProvider_SearchTagsFilter(t *testing.T) {
	p, backend, _ := newProvider(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	now := time.Now()
	putIndexedFull(t, p, backend, scope, "alpha", "a-1", "shared search term", []string{"red"}, now)
	putIndexedFull(t, p, backend, scope, "alpha", "a-2", "shared search term", []string{"blue"}, now)

	res, err := p.Search(ctx, memory.SearchRequest{
		Scopes: []memory.Scope{scope}, Text: "shared", Tags: []string{"red"},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-1", res.Entries[0].Entry.ID)
}

func TestProvider_SearchKindsFilter(t *testing.T) {
	p, backend, _ := newProvider(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	now := time.Now()
	putIndexedFull(t, p, backend, scope, "alpha", "a-1", "shared search term", nil, now)
	putIndexedFull(t, p, backend, scope, "beta", "b-1", "shared search term", nil, now)

	res, err := p.Search(ctx, memory.SearchRequest{
		Scopes: []memory.Scope{scope}, Text: "shared", Kinds: []string{"alpha"},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-1", res.Entries[0].Entry.ID)
}

func TestProvider_SearchSinceFilterExcludesOlderEntry(t *testing.T) {
	p, backend, _ := newProvider(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	older := time.Now().Add(-1 * time.Hour)
	newer := time.Now()
	putIndexedFull(t, p, backend, scope, "alpha", "a-old", "shared search term", nil, older)
	putIndexedFull(t, p, backend, scope, "alpha", "a-new", "shared search term", nil, newer)

	since := time.Now().Add(-10 * time.Minute)
	res, err := p.Search(ctx, memory.SearchRequest{
		Scopes: []memory.Scope{scope}, Text: "shared", Since: &since,
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "a-new", res.Entries[0].Entry.ID)
}

// TestProvider_SearchFieldsAreDropped covers the FieldFilters==false half
// of Finding 2: Search does not evaluate content field predicates, so it
// must surface each requested field path via DroppedFilters rather than
// silently ignoring it or (worse) silently honoring it partially.
func TestProvider_SearchFieldsAreDropped(t *testing.T) {
	p, backend, _ := newProvider(t)
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	putIndexed(t, p, backend, scope, "alpha", "a-1", "shared search term")

	res, err := p.Search(ctx, memory.SearchRequest{
		Scopes: []memory.Scope{scope}, Text: "shared",
		Fields: []memory.FieldFilter{{Path: "status", Value: "active"}},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1, "the row still matches on text; the field predicate is dropped, not silently applied")
	assert.Equal(t, []string{"status"}, res.DroppedFilters)
	assert.False(t, p.SearchCapabilities().FieldFilters)
}
