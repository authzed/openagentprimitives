package search_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/search"
)

func entry(kind, id string) memory.Entry {
	return memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"},
		Kind:  kind,
		ID:    id,
	}
}

func TestRRFRanker_SingleProvider(t *testing.T) {
	r := &search.RRFRanker{K: 60}
	results := map[string]memory.SearchResult{
		"pg": {Entries: []memory.ScoredEntry{
			{Entry: entry("fact", "fact-1"), Score: 0.9, Source: "pg"},
			{Entry: entry("fact", "fact-2"), Score: 0.7, Source: "pg"},
			{Entry: entry("fact", "fact-3"), Score: 0.5, Source: "pg"},
		}},
	}
	ranked := r.Rank(results, 10)
	require.Len(t, ranked, 3)
	assert.Equal(t, "fact-1", ranked[0].Entry.ID)
	assert.Equal(t, "fact-2", ranked[1].Entry.ID)
	assert.Equal(t, "fact-3", ranked[2].Entry.ID)
	for _, se := range ranked {
		assert.Greater(t, se.Score, 0.0)
		assert.LessOrEqual(t, se.Score, 1.0)
	}
}

func TestRRFRanker_MultiProviderBoost(t *testing.T) {
	r := &search.RRFRanker{K: 60}
	results := map[string]memory.SearchResult{
		"pg": {Entries: []memory.ScoredEntry{
			{Entry: entry("entity", "ent-shared"), Score: 0.8, Source: "pg"},
			{Entry: entry("entity", "ent-pg-only"), Score: 0.9, Source: "pg"},
		}},
		"graphiti": {Entries: []memory.ScoredEntry{
			{Entry: entry("entity", "ent-shared"), Score: 0.7, Source: "graphiti"},
			{Entry: entry("entity", "ent-graph-only"), Score: 0.95, Source: "graphiti"},
		}},
	}
	ranked := r.Rank(results, 10)
	require.Len(t, ranked, 3, "3 unique entries after dedup")
	assert.Equal(t, "ent-shared", ranked[0].Entry.ID,
		"entry appearing in both providers should rank first due to RRF boost")
	assert.Contains(t, ranked[0].Source, "+",
		"multi-provider entry source should join provider names with +")
}

func TestRRFRanker_LimitTruncates(t *testing.T) {
	r := &search.RRFRanker{K: 60}
	results := map[string]memory.SearchResult{
		"pg": {Entries: []memory.ScoredEntry{
			{Entry: entry("fact", "fact-1"), Score: 0.9, Source: "pg"},
			{Entry: entry("fact", "fact-2"), Score: 0.8, Source: "pg"},
			{Entry: entry("fact", "fact-3"), Score: 0.7, Source: "pg"},
		}},
	}
	ranked := r.Rank(results, 2)
	assert.Len(t, ranked, 2)
}

func TestRRFRanker_EmptyInput(t *testing.T) {
	r := &search.RRFRanker{K: 60}
	ranked := r.Rank(map[string]memory.SearchResult{}, 10)
	assert.Empty(t, ranked)
}

func TestRRFRanker_DefaultK(t *testing.T) {
	r := &search.RRFRanker{}
	results := map[string]memory.SearchResult{
		"pg": {Entries: []memory.ScoredEntry{
			{Entry: entry("fact", "fact-1"), Score: 0.9, Source: "pg"},
		}},
	}
	ranked := r.Rank(results, 10)
	require.Len(t, ranked, 1)
	assert.Greater(t, ranked[0].Score, 0.0)
}
