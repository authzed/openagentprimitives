package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// PostgresSearchProvider implements memory.SearchProvider using PostgreSQL
// full-text search (tsvector) and optionally pgvector for embedding-based
// similarity search.
type PostgresSearchProvider struct {
	pool     *pgxpool.Pool
	embedder memory.EmbeddingProvider
}

var _ memory.SearchProvider = (*PostgresSearchProvider)(nil)

// New creates a PostgresSearchProvider. pool and embedder may be nil (useful
// in tests that only exercise BuildSearchQuery).
func New(pool *pgxpool.Pool, embedder memory.EmbeddingProvider) *PostgresSearchProvider {
	return &PostgresSearchProvider{pool: pool, embedder: embedder}
}

func (*PostgresSearchProvider) Name() string { return "postgres" }

func (p *PostgresSearchProvider) SearchCapabilities() memory.SearchCapabilities {
	return memory.SearchCapabilities{
		TextSearch:    true,
		VectorSearch:  p.embedder != nil,
		FieldFilters:  true,
		TagFilters:    true,
		TimeRange:     true,
		LinkTraversal: true,
	}
}

func (p *PostgresSearchProvider) Search(ctx context.Context, req memory.SearchRequest) (memory.SearchResult, error) {
	hasVector := req.Text != "" && p.embedder != nil
	var embedding []float32
	if hasVector {
		var err error
		embedding, err = p.embedder.Embed(ctx, req.Text)
		if err != nil {
			slog.Info("postgres search: embedding failed, falling back to text-only", "err", err.Error())
			hasVector = false
		}
	}

	sql, args, dropped := BuildSearchQuery(req, hasVector)
	if hasVector {
		// Slot 0 was reserved by BuildSearchQuery for the vector parameter.
		args = append([]interface{}{pgVectorLiteral(embedding)}, args...)
	}

	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return memory.SearchResult{}, fmt.Errorf("postgres search: %w", err)
	}
	defer rows.Close()

	var entries []memory.ScoredEntry
	for rows.Next() {
		var (
			e         memory.Entry
			linksJSON []byte
			score     float64
		)
		if err := rows.Scan(
			&e.Scope.Kind, &e.Scope.ID, &e.Kind, &e.ID,
			&e.CreatedAt, &e.Tags, &e.Content, &linksJSON, &score,
		); err != nil {
			return memory.SearchResult{}, fmt.Errorf("postgres search scan: %w", err)
		}
		if linksJSON != nil {
			_ = json.Unmarshal(linksJSON, &e.Links)
		}
		entries = append(entries, memory.ScoredEntry{
			Entry:  e,
			Score:  score,
			Source: "postgres",
		})
	}
	return memory.SearchResult{Entries: entries, DroppedFilters: dropped}, rows.Err()
}

func (p *PostgresSearchProvider) Index(ctx context.Context, scope memory.Scope, kind, id string, content []byte) error {
	if p.pool == nil || p.embedder == nil {
		return nil
	}
	embedding, err := p.embedder.Embed(ctx, string(content))
	if err != nil {
		return fmt.Errorf("postgres index embed: %w", err)
	}
	_, err = p.pool.Exec(ctx,
		"UPDATE memory_entry SET embedding = $1 WHERE scope_kind = $2 AND scope_id = $3 AND kind = $4 AND entry_id = $5",
		pgVectorLiteral(embedding), scope.Kind, scope.ID, kind, id)
	return err
}

func (*PostgresSearchProvider) DeleteIndex(_ context.Context, _ memory.Scope, _, _ string) error {
	return nil
}

func (*PostgresSearchProvider) DeleteScopeIndex(_ context.Context, _ memory.Scope) error {
	return nil
}

func pgVectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%f", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

type queryBuilder struct {
	args   []interface{}
	argIdx int
}

func (b *queryBuilder) next(val interface{}) string {
	b.argIdx++
	b.args = append(b.args, val)
	return fmt.Sprintf("$%d", b.argIdx)
}

// BuildSearchQuery constructs the search SQL and its argument list. With
// hasVector true the function RESERVES the vector parameter slot and the caller
// MUST append the vector literal as the last element of the returned args.
//
// The third result names filters the query could not honor, for
// SearchResult.DroppedFilters: a silently discarded filter widens the result
// set, and the caller must be able to tell that from a genuinely unfiltered
// answer. Exported so it can be unit-tested without a database.
func BuildSearchQuery(req memory.SearchRequest, hasVector bool) (string, []interface{}, []string) {
	b := &queryBuilder{}
	hasText := req.Text != ""
	var dropped []string

	// When vector search is active, reserve the vector parameter slot
	// first so its $N is known when building the SELECT expression.
	var vecParam string
	if hasVector {
		b.argIdx++
		vecParam = fmt.Sprintf("$%d", b.argIdx)
	}

	var selectCols string
	switch {
	case hasText && hasVector:
		textParam := b.next(req.Text)
		selectCols = fmt.Sprintf(
			"scope_kind, scope_id, kind, entry_id, created_at, tags, content, links, "+
				"(COALESCE(ts_rank(content_tsv, plainto_tsquery('english', %s)), 0) * 0.4 + "+
				"COALESCE(1.0 - (embedding <=> %s::vector), 0) * 0.6) AS score",
			textParam, vecParam)
	case hasText:
		textParam := b.next(req.Text)
		selectCols = fmt.Sprintf(
			"scope_kind, scope_id, kind, entry_id, created_at, tags, content, links, "+
				"COALESCE(ts_rank(content_tsv, plainto_tsquery('english', %s)), 0) AS score",
			textParam)
	default:
		selectCols = "scope_kind, scope_id, kind, entry_id, created_at, tags, content, links, 1.0 AS score"
	}

	var where []string
	if len(req.Scopes) == 0 {
		// Fail CLOSED: no scopes must match nothing, never every scope in the
		// table. The authorization door upstream approves scopes one by one, so
		// an empty list carries no approval to match against.
		where = append(where, "FALSE")
	} else if len(req.Scopes) == 1 {
		s := req.Scopes[0]
		where = append(where, fmt.Sprintf("(scope_kind = %s AND scope_id = %s)", b.next(s.Kind), b.next(s.ID)))
	} else {
		var scopeClauses []string
		for _, s := range req.Scopes {
			scopeClauses = append(scopeClauses,
				fmt.Sprintf("(scope_kind = %s AND scope_id = %s)", b.next(s.Kind), b.next(s.ID)))
		}
		where = append(where, "("+strings.Join(scopeClauses, " OR ")+")")
	}

	switch {
	case hasVector:
		// hasVector implies hasText, but requiring the full-text match too would
		// reduce the blend to a re-ranking of lexical hits: a semantically close
		// yet lexically disjoint row would be filtered out before its vector
		// distance was ever scored. Admit rows matching lexically OR carrying an
		// embedding and let the blended ORDER BY … LIMIT rank them; the scope
		// predicate above still bounds candidates to the authorized scopes.
		where = append(where, fmt.Sprintf(
			"(content_tsv @@ plainto_tsquery('english', %s) OR embedding IS NOT NULL)", b.next(req.Text)))
	case hasText:
		where = append(where, fmt.Sprintf("content_tsv @@ plainto_tsquery('english', %s)", b.next(req.Text)))
	}
	if len(req.Kinds) > 0 {
		where = append(where, fmt.Sprintf("kind = ANY(%s)", b.next(req.Kinds)))
	}
	if len(req.Tags) > 0 {
		where = append(where, fmt.Sprintf("tags @> %s::text[]", b.next(req.Tags)))
	}
	if req.Since != nil {
		where = append(where, fmt.Sprintf("created_at >= %s", b.next(req.Since.UTC())))
	}
	if req.Until != nil {
		where = append(where, fmt.Sprintf("created_at < %s", b.next(req.Until.UTC())))
	}
	for _, ff := range req.Fields {
		if !memory.SafeFieldPath(ff.Path) {
			// The path itself is caller-supplied and rejected, so it is not
			// echoed back into the report.
			dropped = append(dropped, "Fields[unsafe path]")
			continue
		}
		op := memory.NormalizeFieldOp(ff.Op)
		col := fmt.Sprintf("content->>'%s'", ff.Path)
		switch op {
		case memory.FieldOpEq:
			where = append(where, fmt.Sprintf("%s = %s", col, b.next(fmt.Sprint(ff.Value))))
		case memory.FieldOpLt:
			where = append(where, fmt.Sprintf("%s < %s", col, b.next(fmt.Sprint(ff.Value))))
		case memory.FieldOpLte:
			where = append(where, fmt.Sprintf("%s <= %s", col, b.next(fmt.Sprint(ff.Value))))
		case memory.FieldOpGt:
			where = append(where, fmt.Sprintf("%s > %s", col, b.next(fmt.Sprint(ff.Value))))
		case memory.FieldOpGte:
			where = append(where, fmt.Sprintf("%s >= %s", col, b.next(fmt.Sprint(ff.Value))))
		case memory.FieldOpIsNil:
			where = append(where, fmt.Sprintf("(content->'%s' IS NULL OR content->'%s' = 'null'::jsonb)", ff.Path, ff.Path))
		case memory.FieldOpNotNil:
			where = append(where, fmt.Sprintf("(content->'%s' IS NOT NULL AND content->'%s' != 'null'::jsonb)", ff.Path, ff.Path))
		default:
			// An op this builder does not render must not vanish: without the
			// predicate the search answers a wider question than was asked.
			// The path passed SafeFieldPath, so it is safe to name; the op is
			// caller-supplied and is not echoed.
			dropped = append(dropped, fmt.Sprintf("Fields[%s]", ff.Path))
		}
	}

	sql := "SELECT " + selectCols + " FROM memory_entry WHERE " + strings.Join(where, " AND ")

	if hasText || hasVector {
		sql += " ORDER BY score DESC"
	} else {
		sql += " ORDER BY created_at DESC"
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	sql += fmt.Sprintf(" LIMIT %s", b.next(limit))

	return sql, b.args, dropped
}
