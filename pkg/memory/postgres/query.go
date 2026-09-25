package postgres

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// selectEntryColumns is the fixed SELECT column list shared by buildQuery,
// buildCrossScopeQuery, and the Get-by-id statement. It MUST stay in sync with
// scanEntryFromRows (which scans them in this exact order: scope_kind, scope_id,
// kind, entry_id, created_at, tags, content, links, provenance).
const selectEntryColumns = "SELECT scope_kind, scope_id, kind, entry_id, created_at, tags, content, links, provenance"

type builtQuery struct {
	sql    string
	args   []interface{}
	argIdx int
}

func newBuilder() *builtQuery {
	return &builtQuery{argIdx: 0}
}

func (b *builtQuery) nextParam(val interface{}) string {
	b.argIdx++
	b.args = append(b.args, val)
	return fmt.Sprintf("$%d", b.argIdx)
}

// buildQuery renders q as SQL. The third result names the predicates the
// query could not honor, for QueryResult.DroppedPredicates — a predicate the
// backend silently discards widens the result set, and the caller has to be
// able to tell that apart from a genuinely unfiltered answer.
func buildQuery(q memory.Query) (string, []interface{}, []string) {
	b := newBuilder()
	var where []string
	var dropped []string

	where = append(where, fmt.Sprintf("scope_kind = %s", b.nextParam(q.Scope.Kind)))
	where = append(where, fmt.Sprintf("scope_id = %s", b.nextParam(q.Scope.ID)))

	if len(q.Kinds) > 0 {
		where = append(where, fmt.Sprintf("kind = ANY(%s)", b.nextParam(q.Kinds)))
	}
	if len(q.IDs) > 0 {
		where = append(where, fmt.Sprintf("entry_id = ANY(%s)", b.nextParam(q.IDs)))
	}
	if len(q.Tags) > 0 {
		where = append(where, fmt.Sprintf("tags @> %s::text[]", b.nextParam(q.Tags)))
	}
	if q.Since != nil {
		where = append(where, fmt.Sprintf("created_at >= %s", b.nextParam(q.Since.UTC())))
	}
	if q.Until != nil {
		where = append(where, fmt.Sprintf("created_at < %s", b.nextParam(q.Until.UTC())))
	}

	for _, ff := range q.FieldEquals {
		if !memory.SafeFieldPath(ff.Path) {
			// The path itself is caller-supplied and rejected, so it is not
			// echoed back into the report.
			dropped = append(dropped, "FieldEquals[unsafe path]")
			continue
		}
		op := memory.NormalizeFieldOp(ff.Op)
		col := fmt.Sprintf("content->>'%s'", ff.Path)
		switch op {
		case memory.FieldOpEq:
			where = append(where, fmt.Sprintf("%s = %s", col, b.nextParam(fmt.Sprint(ff.Value))))
		case memory.FieldOpLt:
			where = append(where, fmt.Sprintf("%s < %s", col, b.nextParam(fmt.Sprint(ff.Value))))
		case memory.FieldOpLte:
			where = append(where, fmt.Sprintf("%s <= %s", col, b.nextParam(fmt.Sprint(ff.Value))))
		case memory.FieldOpGt:
			where = append(where, fmt.Sprintf("%s > %s", col, b.nextParam(fmt.Sprint(ff.Value))))
		case memory.FieldOpGte:
			where = append(where, fmt.Sprintf("%s >= %s", col, b.nextParam(fmt.Sprint(ff.Value))))
		case memory.FieldOpIsNil:
			where = append(where, fmt.Sprintf("(content->'%s' IS NULL OR content->'%s' = 'null'::jsonb)", ff.Path, ff.Path))
		case memory.FieldOpNotNil:
			where = append(where, fmt.Sprintf("(content->'%s' IS NOT NULL AND content->'%s' != 'null'::jsonb)", ff.Path, ff.Path))
		default:
			// An op this builder does not render must not vanish: without the
			// predicate the query answers a wider question than was asked.
			// The path passed SafeFieldPath, so it is safe to name; the op is
			// caller-supplied and is not echoed.
			dropped = append(dropped, fmt.Sprintf("FieldEquals[%s]", ff.Path))
		}
	}

	for _, lt := range q.LinkedTo {
		sub := fmt.Sprintf(
			"entry_id IN (SELECT source_id FROM memory_link WHERE source_scope_kind = %s AND source_scope_id = %s AND target_kind = %s AND target_id = %s",
			b.nextParam(q.Scope.Kind), b.nextParam(q.Scope.ID),
			b.nextParam(lt.Kind), b.nextParam(lt.ID))
		if lt.Relation != "" {
			sub += fmt.Sprintf(" AND relation = %s", b.nextParam(lt.Relation))
		}
		sub += ")"
		where = append(where, sub)
	}

	for _, lf := range q.LinkedFrom {
		sub := fmt.Sprintf(
			"entry_id IN (SELECT target_id FROM memory_link WHERE target_scope_kind = %s AND target_scope_id = %s AND source_kind = %s AND source_id = %s",
			b.nextParam(q.Scope.Kind), b.nextParam(q.Scope.ID),
			b.nextParam(lf.Kind), b.nextParam(lf.ID))
		if lf.Relation != "" {
			sub += fmt.Sprintf(" AND relation = %s", b.nextParam(lf.Relation))
		}
		sub += ")"
		where = append(where, sub)
	}

	sql := selectEntryColumns + " FROM memory_entry WHERE " + strings.Join(where, " AND ")

	switch {
	case q.OrderBy.Field == "createdAt" && q.OrderBy.Desc:
		sql += " ORDER BY created_at DESC"
	case q.OrderBy.Field == "createdAt":
		sql += " ORDER BY created_at ASC"
	case q.Limit > 0:
		// A LIMIT with no ORDER BY lets Postgres hand back any N of the
		// matching rows, and a different N next time — the caller cannot page
		// and cannot reproduce the answer. Default to newest-first with the
		// entry ID breaking ties, the same total order buildCrossScopeQuery
		// uses for exactly this reason. Unlimited queries stay unordered:
		// nothing is discarded, so nothing needs sorting.
		sql += " ORDER BY created_at DESC, entry_id ASC"
	}
	if q.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %s", b.nextParam(q.Limit))
	}

	return sql, b.args, dropped
}

// buildCrossScopeQuery builds the admin cross-scope SQL: kind+time filtered
// across every scope of one scope_kind, ordered before LIMIT/OFFSET.
func buildCrossScopeQuery(q memory.CrossScopeQuery) (string, []interface{}) {
	b := newBuilder()
	var where []string
	where = append(where, fmt.Sprintf("scope_kind = %s", b.nextParam(q.ScopeKind)))
	where = append(where, fmt.Sprintf("kind = ANY(%s)", b.nextParam(q.Kinds)))
	if q.Since != nil {
		where = append(where, fmt.Sprintf("created_at >= %s", b.nextParam(q.Since.UTC())))
	}
	if q.Until != nil {
		where = append(where, fmt.Sprintf("created_at < %s", b.nextParam(q.Until.UTC())))
	}
	// The column list MUST match buildQuery's SELECT verbatim, because
	// scanEntryFromRows decodes both. Reuse selectEntryColumns rather than
	// hand-listing: when the columns change, scanEntry changes with them and
	// this stays correct.
	sql := selectEntryColumns + " FROM memory_entry WHERE " +
		strings.Join(where, " AND ")
	// Deterministic total order so pagination never duplicates/skips.
	if q.OrderDesc {
		sql += " ORDER BY created_at DESC, scope_id ASC, entry_id ASC"
	} else {
		sql += " ORDER BY created_at ASC, scope_id ASC, entry_id ASC"
	}
	if q.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %s", b.nextParam(q.Limit))
	}
	if q.Offset > 0 {
		sql += fmt.Sprintf(" OFFSET %s", b.nextParam(q.Offset))
	}
	return sql, b.args
}
