package sqlite

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// selectEntryColumns is the fixed SELECT column list shared by buildQuery,
// buildCrossScopeQuery, and Get. It MUST stay in sync with scanEntry, which
// scans in exactly this order. Dropping provenance here would make entries
// round-trip "unsigned", which the chain verifier reports as a warning rather
// than the hard finding a genuinely missing signature deserves.
const selectEntryColumns = "SELECT scope_kind, scope_id, kind, entry_id, created_at, tags, content, links, provenance"

// buildQuery translates a memory.Query into SQLite SQL and ? args, mirroring
// the postgres builder: link filters are single-hop subqueries, tag filters
// require ALL listed tags, and FieldEquals uses the ->> top-level-key extractor.
//
// SQLite's ->> returns a NATIVELY-TYPED value (INTEGER/REAL/TEXT) where
// postgres's jsonb ->> always returns TEXT. Comparing the typed value against a
// TEXT-bound parameter fails under SQLite's storage-class ordering
// (NUMERIC < TEXT), so every comparison casts to TEXT first — reproducing
// postgres's semantics, lexicographic-not-numeric quirk included, instead of a
// silently broken numeric comparison.
//
// The third result names predicates the query could not honor, for
// QueryResult.DroppedPredicates: a silently discarded predicate widens the
// result set, and the caller must be able to tell that from a genuinely
// unfiltered answer.
func buildQuery(q memory.Query) (string, []any, []string) {
	var (
		where   []string
		args    []any
		dropped []string
	)
	add := func(clause string, vals ...any) {
		where = append(where, clause)
		args = append(args, vals...)
	}

	add("scope_kind = ?", q.Scope.Kind)
	add("scope_id = ?", q.Scope.ID)

	if len(q.Kinds) > 0 {
		add("kind IN ("+placeholders(len(q.Kinds))+")", toAny(q.Kinds)...)
	}
	if len(q.IDs) > 0 {
		add("entry_id IN ("+placeholders(len(q.IDs))+")", toAny(q.IDs)...)
	}
	for _, tag := range q.Tags {
		add("EXISTS (SELECT 1 FROM json_each(memory_entry.tags) WHERE value = ?)", tag)
	}
	if q.Since != nil {
		add("created_at >= ?", q.Since.UTC().UnixNano())
	}
	if q.Until != nil {
		add("created_at < ?", q.Until.UTC().UnixNano())
	}

	for _, ff := range q.FieldEquals {
		if !memory.SafeFieldPath(ff.Path) {
			// The path itself is caller-supplied and rejected, so it is not
			// echoed back into the report.
			dropped = append(dropped, "FieldEquals[unsafe path]")
			continue
		}
		switch memory.NormalizeFieldOp(ff.Op) {
		case memory.FieldOpEq:
			add("CAST(content ->> ? AS TEXT) = ?", ff.Path, fmt.Sprint(ff.Value))
		case memory.FieldOpLt:
			add("CAST(content ->> ? AS TEXT) < ?", ff.Path, fmt.Sprint(ff.Value))
		case memory.FieldOpLte:
			add("CAST(content ->> ? AS TEXT) <= ?", ff.Path, fmt.Sprint(ff.Value))
		case memory.FieldOpGt:
			add("CAST(content ->> ? AS TEXT) > ?", ff.Path, fmt.Sprint(ff.Value))
		case memory.FieldOpGte:
			add("CAST(content ->> ? AS TEXT) >= ?", ff.Path, fmt.Sprint(ff.Value))
		case memory.FieldOpIsNil:
			add("content ->> ? IS NULL", ff.Path)
		case memory.FieldOpNotNil:
			add("content ->> ? IS NOT NULL", ff.Path)
		default:
			// An op this builder does not render must not vanish: without the
			// predicate the query answers a wider question than was asked.
			// The path passed SafeFieldPath, so it is safe to name; the op is
			// caller-supplied and is not echoed.
			dropped = append(dropped, fmt.Sprintf("FieldEquals[%s]", ff.Path))
		}
	}

	for _, lt := range q.LinkedTo {
		clause := "entry_id IN (SELECT source_id FROM memory_link WHERE source_scope_kind = ? AND source_scope_id = ? AND target_kind = ? AND target_id = ?"
		vals := []any{q.Scope.Kind, q.Scope.ID, lt.Kind, lt.ID}
		if lt.Relation != "" {
			clause += " AND relation = ?"
			vals = append(vals, lt.Relation)
		}
		clause += ")"
		add(clause, vals...)
	}
	for _, lf := range q.LinkedFrom {
		clause := "entry_id IN (SELECT target_id FROM memory_link WHERE target_scope_kind = ? AND target_scope_id = ? AND source_kind = ? AND source_id = ?"
		vals := []any{q.Scope.Kind, q.Scope.ID, lf.Kind, lf.ID}
		if lf.Relation != "" {
			clause += " AND relation = ?"
			vals = append(vals, lf.Relation)
		}
		clause += ")"
		add(clause, vals...)
	}

	sql := selectEntryColumns + " FROM memory_entry WHERE " +
		strings.Join(where, " AND ")

	if q.OrderBy.Field == "createdAt" {
		if q.OrderBy.Desc {
			sql += " ORDER BY created_at DESC"
		} else {
			sql += " ORDER BY created_at ASC"
		}
	}
	if q.Limit > 0 {
		sql += " LIMIT ?"
		args = append(args, q.Limit)
	}
	return sql, args, dropped
}

// buildCrossScopeQuery builds the admin cross-scope SQL: kind+time filtered
// across every scope of one scope_kind, ordered before LIMIT/OFFSET. It
// mirrors postgres's buildCrossScopeQuery (same WHERE shape, same
// deterministic tie-break order), swapping $N placeholders for ? and
// ANY($) for IN (...).
func buildCrossScopeQuery(q memory.CrossScopeQuery) (string, []any) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, vals ...any) {
		where = append(where, clause)
		args = append(args, vals...)
	}

	add("scope_kind = ?", q.ScopeKind)
	add("kind IN ("+placeholders(len(q.Kinds))+")", toAny(q.Kinds)...)
	if q.Since != nil {
		add("created_at >= ?", q.Since.UTC().UnixNano())
	}
	if q.Until != nil {
		add("created_at < ?", q.Until.UTC().UnixNano())
	}

	// IMPORTANT: the column list MUST match buildQuery's SELECT verbatim
	// (both share selectEntryColumns) because we reuse scanEntry below to
	// decode rows.
	sql := selectEntryColumns + " FROM memory_entry WHERE " +
		strings.Join(where, " AND ")

	// Deterministic total order so pagination never duplicates/skips.
	if q.OrderDesc {
		sql += " ORDER BY created_at DESC, scope_id ASC, entry_id ASC"
	} else {
		sql += " ORDER BY created_at ASC, scope_id ASC, entry_id ASC"
	}
	if q.Limit > 0 {
		sql += " LIMIT ?"
		args = append(args, q.Limit)
	}
	if q.Offset > 0 {
		sql += " OFFSET ?"
		args = append(args, q.Offset)
	}
	return sql, args
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
