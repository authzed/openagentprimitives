package graphiti_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/search/graphiti"
)

func TestGraphitiProvider_Name(t *testing.T) {
	p := graphiti.New("http://localhost:8000")
	assert.Equal(t, "graphiti", p.Name())
}

func TestGraphitiProvider_Capabilities(t *testing.T) {
	p := graphiti.New("http://localhost:8000")
	caps := p.SearchCapabilities()
	assert.True(t, caps.TextSearch)
	assert.True(t, caps.VectorSearch)
	assert.True(t, caps.LinkTraversal)
	assert.False(t, caps.FieldFilters)
	assert.False(t, caps.TagFilters)
}

func TestGraphitiProvider_Search(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/search", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(graphiti.GraphitiSearchResponse{
			Facts: []graphiti.GraphitiResult{
				{UUID: "abc123", Name: "WORKS_AT", Fact: "Bob works at TechCorp"},
				{UUID: "def456", Name: "LIVES_IN", Fact: "Bob lives in SF"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	p := graphiti.New(srv.URL)
	res, err := p.Search(context.Background(), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Text:   "Bob's job",
		Limit:  10,
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)
	assert.Equal(t, "fact", res.Entries[0].Entry.Kind)
	assert.Contains(t, res.Entries[0].Entry.ID, "abc123")
	assert.Equal(t, "graphiti", res.Entries[0].Source)
	assert.Greater(t, res.Entries[0].Score, 0.0)
}

func TestGraphitiProvider_SearchEmptyGraph(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(graphiti.GraphitiSearchResponse{Facts: []graphiti.GraphitiResult{}})
	}))
	t.Cleanup(srv.Close)

	p := graphiti.New(srv.URL)
	res, err := p.Search(context.Background(), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Text:   "test",
	})
	require.NoError(t, err)
	assert.Empty(t, res.Entries)
}

func TestGraphitiProvider_SearchServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	p := graphiti.New(srv.URL)
	_, err := p.Search(context.Background(), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Text:   "test",
	})
	require.Error(t, err)
}

func TestGraphitiProvider_IndexNoOp(t *testing.T) {
	p := graphiti.New("http://localhost:8000")
	assert.NoError(t, p.Index(context.Background(), memory.Scope{}, "", "", nil))
	assert.NoError(t, p.DeleteIndex(context.Background(), memory.Scope{}, "", ""))
	assert.NoError(t, p.DeleteScopeIndex(context.Background(), memory.Scope{}))
}
