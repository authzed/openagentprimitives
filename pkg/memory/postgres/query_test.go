package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestBuildQuery_BasicScope(t *testing.T) {
	q := memory.Query{Scope: memory.Scope{Kind: "session", ID: "ns/a"}}
	sql, args, _ := buildQuery(q)
	assert.Contains(t, sql, "scope_kind = $1")
	assert.Contains(t, sql, "scope_id = $2")
	assert.Equal(t, "session", args[0])
	assert.Equal(t, "ns/a", args[1])
}

func TestBuildQuery_KindFilter(t *testing.T) {
	q := memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kinds: []string{"turn", "label"},
	}
	sql, args, _ := buildQuery(q)
	assert.Contains(t, sql, "kind = ANY($3)")
	assert.Equal(t, []string{"turn", "label"}, args[2])
}

func TestBuildQuery_Tags(t *testing.T) {
	q := memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Tags:  []string{"important", "urgent"},
	}
	sql, args, _ := buildQuery(q)
	assert.Contains(t, sql, "tags @> $3::text[]")
	assert.Equal(t, []string{"important", "urgent"}, args[2])
}

func TestBuildQuery_TimeRange(t *testing.T) {
	now := time.Now().UTC()
	q := memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Since: &now,
	}
	sql, _, _ := buildQuery(q)
	assert.Contains(t, sql, "created_at >= $3")
}

func TestBuildQuery_FieldRange(t *testing.T) {
	q := memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		FieldEquals: []memory.FieldFilter{
			{Path: "valid_at", Op: memory.FieldOpLte, Value: "2024-01-01"},
			{Path: "invalid_at", Op: memory.FieldOpIsNil},
		},
	}
	sql, args, _ := buildQuery(q)
	assert.Contains(t, sql, "content->>'valid_at' <= $3")
	assert.Equal(t, "2024-01-01", args[2])
	assert.Contains(t, sql, "content->'invalid_at' IS NULL OR content->'invalid_at' = 'null'::jsonb")
}

// A predicate the builder cannot render must be REPORTED, not discarded: the
// query then answers a wider question than the caller asked, and
// QueryResult.DroppedPredicates is the signal every caller — including the
// agent's query_memory tool, which relays it to the model — reads to tell the
// two apart. An unrecognized FieldOp is reachable today: query_memory passes
// memory.FieldOp(ff.Op) straight through from the model.
func TestBuildQuery_UnhonoredFieldPredicatesAreReported(t *testing.T) {
	cases := []struct {
		name    string
		filter  memory.FieldFilter
		wantSQL string
	}{
		{
			name:    "unrecognized op: reported, not silently dropped",
			filter:  memory.FieldFilter{Path: "decision", Op: memory.FieldOp("contains"), Value: "approved"},
			wantSQL: "FieldEquals[decision]",
		},
		{
			name:    "unsafe field path: reported, and never interpolated",
			filter:  memory.FieldFilter{Path: "content'; DROP TABLE memory_entry; --", Value: "x"},
			wantSQL: "FieldEquals[unsafe path]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := memory.Query{
				Scope:       memory.Scope{Kind: "session", ID: "ns/a"},
				FieldEquals: []memory.FieldFilter{tc.filter},
			}
			sql, _, dropped := buildQuery(q)
			assert.Equal(t, []string{tc.wantSQL}, dropped)
			assert.NotContains(t, sql, "DROP TABLE")
		})
	}
}

func TestBuildQuery_HonoredFieldPredicateIsNotReportedAsDropped(t *testing.T) {
	q := memory.Query{
		Scope:       memory.Scope{Kind: "session", ID: "ns/a"},
		FieldEquals: []memory.FieldFilter{{Path: "decision", Value: "approved"}},
	}
	sql, _, dropped := buildQuery(q)
	assert.Empty(t, dropped)
	assert.Contains(t, sql, "content->>'decision' = $3")
}

func TestBuildQuery_OrderAndLimit(t *testing.T) {
	q := memory.Query{
		Scope:   memory.Scope{Kind: "session", ID: "ns/a"},
		OrderBy: memory.OrderBy{Field: "createdAt", Desc: true},
		Limit:   10,
	}
	sql, args, _ := buildQuery(q)
	assert.Contains(t, sql, "ORDER BY created_at DESC")
	assert.Contains(t, sql, "LIMIT $3")
	assert.Equal(t, 10, args[2])
}

// A LIMIT with no ORDER BY lets Postgres return any N of the matching rows,
// and a different N next time. `oap memory list` / `oap memory query` send
// exactly that shape — Limit set, OrderBy zero.
func TestBuildQuery_LimitWithoutOrderByStillOrders(t *testing.T) {
	q := memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Limit: 10,
	}
	sql, _, _ := buildQuery(q)
	assert.Contains(t, sql, "ORDER BY created_at DESC, entry_id ASC",
		"a truncating query must have a total order to truncate")
	assert.Less(t, strings.Index(sql, "ORDER BY"), strings.Index(sql, "LIMIT"))
}

func TestBuildQuery_NoLimitNoOrderByStaysUnordered(t *testing.T) {
	q := memory.Query{Scope: memory.Scope{Kind: "session", ID: "ns/a"}}
	sql, _, _ := buildQuery(q)
	assert.NotContains(t, sql, "ORDER BY", "nothing is discarded, so nothing needs sorting")
}

func TestBuildQuery_LinkedTo(t *testing.T) {
	q := memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		LinkedTo: []memory.LinkFilter{
			{Kind: "issue", ID: "i-1", Relation: "references"},
		},
	}
	sql, args, _ := buildQuery(q)
	// $3=source_scope_kind, $4=source_scope_id, $5=target_kind, $6=target_id, $7=relation
	assert.Contains(t, sql, "source_scope_kind = $3 AND source_scope_id = $4 AND target_kind = $5 AND target_id = $6 AND relation = $7")
	assert.Equal(t, "session", args[2])
	assert.Equal(t, "ns/a", args[3])
	assert.Equal(t, "issue", args[4])
	assert.Equal(t, "i-1", args[5])
	assert.Equal(t, "references", args[6])
}

func TestBuildQuery_LinkedFrom(t *testing.T) {
	q := memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		LinkedFrom: []memory.LinkFilter{
			{Kind: "turn", ID: "t-1"},
		},
	}
	sql, _, _ := buildQuery(q)
	// $3=target_scope_kind, $4=target_scope_id, $5=source_kind, $6=source_id
	assert.Contains(t, sql, "target_scope_kind = $3 AND target_scope_id = $4 AND source_kind = $5 AND source_id = $6")
}

func TestBuildQuery_ParamNumbering(t *testing.T) {
	now := time.Now().UTC()
	q := memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kinds: []string{"alpha"},
		Tags:  []string{"x"},
		Since: &now,
		LinkedTo: []memory.LinkFilter{
			{Kind: "issue", ID: "i-1"},
		},
		OrderBy: memory.OrderBy{Field: "createdAt"},
		Limit:   5,
	}
	sql, args, _ := buildQuery(q)
	// $1=scope_kind, $2=scope_id, $3=kinds, $4=tags, $5=since,
	// $6=lt_source_scope_kind, $7=lt_source_scope_id, $8=lt_kind, $9=lt_id, $10=limit
	assert.Equal(t, 10, len(args))
	assert.Contains(t, sql, "LIMIT $10")
}
