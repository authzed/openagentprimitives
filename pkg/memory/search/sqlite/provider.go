package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
)

// SearchProvider implements memory.SearchProvider using SQLite FTS5
// lexical search. It shares the backend's *sql.DB (via the Client) so
// it can join memory_fts back to memory_entry.
type SearchProvider struct {
	client *memsqlite.Client
}

var _ memory.SearchProvider = (*SearchProvider)(nil)

// New creates the provider and ensures the FTS5 table exists.
func New(client *memsqlite.Client) (*SearchProvider, error) {
	if _, err := client.DB().ExecContext(context.Background(), ftsMigrateSQL); err != nil {
		return nil, fmt.Errorf("sqlite search: fts migrate: %w", err)
	}
	return &SearchProvider{client: client}, nil
}

func (*SearchProvider) Name() string { return "sqlite" }

func (*SearchProvider) SearchCapabilities() memory.SearchCapabilities {
	return memory.SearchCapabilities{
		TextSearch: true,
		// FieldFilters is false: Search honors Kinds/Tags/Since/Until as
		// extra WHERE clauses on the joined memory_entry row, but does NOT
		// evaluate req.Fields content predicates — those are reported via
		// SearchResult.DroppedFilters instead (see Search below).
		FieldFilters: false,
		TagFilters:   true,
		TimeRange:    true,
		VectorSearch: false,
	}
}

// Index maintains the FTS row for (scope, kind, id) via delete-then-insert.
func (p *SearchProvider) Index(ctx context.Context, scope memory.Scope, kind, id string, content []byte) error {
	tx, err := p.client.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite search index begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err = tx.ExecContext(ctx,
		"DELETE FROM memory_fts WHERE scope_kind=? AND scope_id=? AND kind=? AND entry_id=?",
		scope.Kind, scope.ID, kind, id); err != nil {
		return fmt.Errorf("sqlite search index delete: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		"INSERT INTO memory_fts (scope_kind, scope_id, kind, entry_id, content) VALUES (?, ?, ?, ?, ?)",
		scope.Kind, scope.ID, kind, id, string(content)); err != nil {
		return fmt.Errorf("sqlite search index insert: %w", err)
	}
	return tx.Commit()
}

func (p *SearchProvider) DeleteIndex(ctx context.Context, scope memory.Scope, kind, id string) error {
	if _, err := p.client.DB().ExecContext(ctx,
		"DELETE FROM memory_fts WHERE scope_kind=? AND scope_id=? AND kind=? AND entry_id=?",
		scope.Kind, scope.ID, kind, id); err != nil {
		return fmt.Errorf("sqlite search delete-index: %w", err)
	}
	return nil
}

func (p *SearchProvider) DeleteScopeIndex(ctx context.Context, scope memory.Scope) error {
	if _, err := p.client.DB().ExecContext(ctx,
		"DELETE FROM memory_fts WHERE scope_kind=? AND scope_id=?", scope.Kind, scope.ID); err != nil {
		return fmt.Errorf("sqlite search delete-scope-index: %w", err)
	}
	return nil
}

// buildMatchQuery converts free-form user text into a SAFE FTS5 MATCH argument.
// FTS5 treats bare input as its own query language — AND/OR/NOT/NEAR, "foo:bar"
// column filters, leading-hyphen negation — and throws a syntax error on text a
// caller expects to search verbatim (an apostrophe, a colon, a leading hyphen).
// Quoting each whitespace-split term, doubling embedded quotes per FTS5's escape
// convention, makes every term a literal the grammar cannot reinterpret.
// Space-separated quoted terms are implicitly AND-ed, matching postgres's
// plainto_tsquery. Returns "" when text tokenizes to zero terms.
func buildMatchQuery(text string) string {
	terms := strings.Fields(text)
	if len(terms) == 0 {
		return ""
	}
	quoted := make([]string, len(terms))
	for i, term := range terms {
		quoted[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " ")
}

// Search runs an FTS5 MATCH over content for the requested scopes, joined back
// to memory_entry to reconstruct Entries and to honor the cheap structured
// filters (Kinds, Tags, Since/Until) as extra WHERE clauses. Honoring them HERE
// is required: CompositeSearcher unions this provider in without post-filtering,
// so a filter skipped here leaks matches past the caller's own predicate.
//
// req.Fields is NOT evaluated; each path is reported in
// SearchResult.DroppedFilters, matching SearchCapabilities.FieldFilters=false.
// bm25 is lower-is-better in SQLite, so the score is negated to higher=better
// for RRF. Empty or non-tokenizing Text returns no rows — this provider is
// lexical-only, and structured-only requests are served by the inmem provider.
func (p *SearchProvider) Search(ctx context.Context, req memory.SearchRequest) (memory.SearchResult, error) {
	if req.Text == "" || len(req.Scopes) == 0 {
		return memory.SearchResult{}, nil
	}
	matchQuery := buildMatchQuery(req.Text)
	if matchQuery == "" {
		return memory.SearchResult{}, nil
	}

	var scopeClauses []string
	args := []any{matchQuery}
	for _, s := range req.Scopes {
		scopeClauses = append(scopeClauses, "(f.scope_kind = ? AND f.scope_id = ?)")
		args = append(args, s.Kind, s.ID)
	}

	var extraClauses []string
	if len(req.Kinds) > 0 {
		extraClauses = append(extraClauses, "e.kind IN ("+sqlPlaceholders(len(req.Kinds))+")")
		for _, k := range req.Kinds {
			args = append(args, k)
		}
	}
	for _, tag := range req.Tags {
		extraClauses = append(extraClauses, "EXISTS (SELECT 1 FROM json_each(e.tags) WHERE value = ?)")
		args = append(args, tag)
	}
	if req.Since != nil {
		extraClauses = append(extraClauses, "e.created_at >= ?")
		args = append(args, req.Since.UTC().UnixNano())
	}
	if req.Until != nil {
		extraClauses = append(extraClauses, "e.created_at < ?")
		args = append(args, req.Until.UTC().UnixNano())
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	args = append(args, limit)

	// Ordinary column references may use the "f" alias, but the MATCH operator
	// and the bm25() auxiliary function MUST name the FTS5 table itself, not the
	// alias: `f MATCH ?` and `bm25(f)` fail with "no such column: f" even though
	// `f.scope_kind` resolves fine.
	sqlStr := `
		SELECT e.scope_kind, e.scope_id, e.kind, e.entry_id, e.created_at, e.tags, e.content, e.links,
		       -bm25(memory_fts) AS score
		FROM memory_fts f
		JOIN memory_entry e
		  ON e.scope_kind = f.scope_kind AND e.scope_id = f.scope_id
		 AND e.kind = f.kind AND e.entry_id = f.entry_id
		WHERE memory_fts MATCH ? AND (` + strings.Join(scopeClauses, " OR ") + `)`
	for _, c := range extraClauses {
		sqlStr += " AND " + c
	}
	sqlStr += `
		ORDER BY score DESC
		LIMIT ?`

	rows, err := p.client.DB().QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return memory.SearchResult{}, fmt.Errorf("sqlite search: %w", err)
	}
	defer rows.Close()

	var entries []memory.ScoredEntry
	for rows.Next() {
		var (
			e          memory.Entry
			createdNS  int64
			tagsRaw    string
			contentRaw []byte
			linksRaw   string
			score      float64
		)
		if err := rows.Scan(&e.Scope.Kind, &e.Scope.ID, &e.Kind, &e.ID,
			&createdNS, &tagsRaw, &contentRaw, &linksRaw, &score); err != nil {
			return memory.SearchResult{}, fmt.Errorf("sqlite search scan: %w", err)
		}
		e.CreatedAt = time.Unix(0, createdNS).UTC()
		var tags []string
		if json.Unmarshal([]byte(tagsRaw), &tags) == nil && len(tags) > 0 {
			e.Tags = tags
		}
		if len(contentRaw) > 0 {
			e.Content = contentRaw
		}
		entries = append(entries, memory.ScoredEntry{Entry: e, Score: score, Source: "sqlite"})
	}
	if err := rows.Err(); err != nil {
		return memory.SearchResult{}, fmt.Errorf("sqlite search rows: %w", err)
	}

	var dropped []string
	for _, f := range req.Fields {
		dropped = append(dropped, f.Path)
	}

	return memory.SearchResult{Entries: entries, DroppedFilters: dropped}, nil
}

func sqlPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
