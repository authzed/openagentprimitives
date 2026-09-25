package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	pgsearch "github.com/authzed/openagentprimitives/pkg/memory/search/postgres"
)

type fakeEmbedder struct {
	vec []float32
	err error
}

func (e *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return e.vec, e.err
}

func TestPostgresProvider_Name(t *testing.T) {
	p := pgsearch.New(nil, nil)
	assert.Equal(t, "postgres", p.Name())
}

func TestPostgresProvider_Capabilities_WithEmbedder(t *testing.T) {
	p := pgsearch.New(nil, &fakeEmbedder{})
	caps := p.SearchCapabilities()
	assert.True(t, caps.TextSearch)
	assert.True(t, caps.VectorSearch)
	assert.True(t, caps.FieldFilters)
	assert.True(t, caps.TagFilters)
	assert.True(t, caps.TimeRange)
}

func TestPostgresProvider_Capabilities_NoEmbedder(t *testing.T) {
	p := pgsearch.New(nil, nil)
	caps := p.SearchCapabilities()
	assert.True(t, caps.TextSearch)
	assert.False(t, caps.VectorSearch)
}

func TestBuildSearchQuery_TextOnly(t *testing.T) {
	sql, args, _ := pgsearch.BuildSearchQuery(memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Text:   "onboarding",
		Limit:  10,
	}, false)
	assert.Contains(t, sql, "ts_rank")
	assert.Contains(t, sql, "plainto_tsquery")
	require.GreaterOrEqual(t, len(args), 3)
}

func TestBuildSearchQuery_StructuredOnly(t *testing.T) {
	sql, args, _ := pgsearch.BuildSearchQuery(memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Kinds:  []string{"fact"},
		Tags:   []string{"active"},
		Limit:  5,
	}, false)
	assert.Contains(t, sql, "kind = ANY")
	assert.Contains(t, sql, "tags @>")
	assert.NotContains(t, sql, "ts_rank")
	require.GreaterOrEqual(t, len(args), 4)
}

// The point of blending pgvector into the score is to SURFACE a row the
// lexical index misses. Keeping the full-text predicate as a mandatory
// conjunct while the blend is active reduces vector search to a re-ranking of
// the full-text matches — a semantically close, lexically disjoint row is
// filtered out before it is ever scored.
func TestBuildSearchQuery_VectorBlend_LexicalMatchIsNotMandatory(t *testing.T) {
	sql, _, _ := pgsearch.BuildSearchQuery(memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Text:   "onboarding",
		Limit:  10,
	}, true)

	assert.Contains(t, sql, "embedding <=>", "the blended score must include the vector distance")
	assert.NotContains(t, sql, "AND content_tsv @@",
		"the full-text predicate must not be a mandatory conjunct when the vector blend is active")
	assert.Contains(t, sql, "OR embedding IS NOT NULL",
		"an embedded row with no lexical match must still be a candidate")
}

// Without an embedder there is nothing to rank a non-matching row by, so the
// full-text predicate stays mandatory.
func TestBuildSearchQuery_TextOnly_LexicalMatchIsMandatory(t *testing.T) {
	sql, _, _ := pgsearch.BuildSearchQuery(memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Text:   "onboarding",
		Limit:  10,
	}, false)

	assert.Contains(t, sql, "AND content_tsv @@")
	assert.NotContains(t, sql, "embedding")
}

// Same contract as the backend's buildQuery: a filter the provider cannot
// render widens the result set, so it must be reported through
// SearchResult.DroppedFilters rather than vanishing.
func TestBuildSearchQuery_UnhonoredFieldFiltersAreReported(t *testing.T) {
	cases := []struct {
		name   string
		filter memory.FieldFilter
		want   string
	}{
		{
			name:   "unrecognized op: reported, not silently dropped",
			filter: memory.FieldFilter{Path: "decision", Op: memory.FieldOp("contains"), Value: "approved"},
			want:   "Fields[decision]",
		},
		{
			name:   "unsafe field path: reported, and never interpolated",
			filter: memory.FieldFilter{Path: "content'; DROP TABLE memory_entry; --", Value: "x"},
			want:   "Fields[unsafe path]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, _, dropped := pgsearch.BuildSearchQuery(memory.SearchRequest{
				Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
				Fields: []memory.FieldFilter{tc.filter},
				Limit:  5,
			}, false)
			assert.Equal(t, []string{tc.want}, dropped)
			assert.NotContains(t, sql, "DROP TABLE")
		})
	}
}

func TestBuildSearchQuery_HonoredFieldFilterIsNotReportedAsDropped(t *testing.T) {
	sql, _, dropped := pgsearch.BuildSearchQuery(memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Fields: []memory.FieldFilter{{Path: "decision", Value: "approved"}},
		Limit:  5,
	}, false)
	assert.Empty(t, dropped)
	assert.Contains(t, sql, "content->>'decision' = ")
}

func TestBuildSearchQuery_MultiScope(t *testing.T) {
	sql, _, _ := pgsearch.BuildSearchQuery(memory.SearchRequest{
		Scopes: []memory.Scope{
			{Kind: "session", ID: "ns/a"},
			{Kind: "user", ID: "sam@example.com"},
		},
		Text:  "test",
		Limit: 10,
	}, false)
	assert.Contains(t, sql, "OR")
}
