package memory_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestSearchRequest_JSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	req := memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Text:   "what happened with Bob",
		Kinds:  []string{"entity", "fact"},
		Tags:   []string{"active"},
		Fields: []memory.FieldFilter{{Path: "status", Op: memory.FieldOpEq, Value: "active"}},
		Since:  &now,
		Limit:  10,
	}
	b, err := json.Marshal(req)
	require.NoError(t, err)

	var got memory.SearchRequest
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, req.Scopes, got.Scopes)
	assert.Equal(t, req.Text, got.Text)
	assert.Equal(t, req.Kinds, got.Kinds)
	assert.Equal(t, req.Limit, got.Limit)
}

func TestScoredEntry_JSONRoundTrip(t *testing.T) {
	se := memory.ScoredEntry{
		Entry:  memory.Entry{Kind: "entity", ID: "ent-1"},
		Score:  0.85,
		Source: "postgres",
	}
	b, err := json.Marshal(se)
	require.NoError(t, err)

	var got memory.ScoredEntry
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, se.Score, got.Score)
	assert.Equal(t, se.Source, got.Source)
	assert.Equal(t, se.Entry.ID, got.Entry.ID)
}

func TestMergedSearchResult_JSONRoundTrip(t *testing.T) {
	mr := memory.MergedSearchResult{
		Entries: []memory.ScoredEntry{
			{Entry: memory.Entry{Kind: "fact", ID: "fact-1"}, Score: 0.9, Source: "postgres"},
		},
		PerProvider: map[string]memory.SearchResult{
			"postgres": {Entries: []memory.ScoredEntry{
				{Entry: memory.Entry{Kind: "fact", ID: "fact-1"}, Score: 0.9, Source: "postgres"},
			}},
		},
	}
	b, err := json.Marshal(mr)
	require.NoError(t, err)

	var got memory.MergedSearchResult
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Len(t, got.Entries, 1)
	assert.Len(t, got.PerProvider, 1)
	assert.Equal(t, 0.9, got.PerProvider["postgres"].Entries[0].Score)
}
