package graphiti_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/search/graphiti"
)

func TestIngest_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/messages", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)

		var req struct {
			GroupID  string `json:"group_id"`
			Messages []struct {
				Content  string `json:"content"`
				RoleType string `json:"role_type"`
			} `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "test-group", req.GroupID)
		require.Len(t, req.Messages, 1)
		assert.Equal(t, "Bob works at TechCorp.", req.Messages[0].Content)
		assert.Equal(t, "user", req.Messages[0].RoleType)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(graphiti.IngestResult{
			Entities: []graphiti.GraphitiResult{
				{Type: "entity", UUID: "ent-1", Name: "Bob", Summary: "Person"},
			},
			Facts: []graphiti.GraphitiResult{
				{Type: "fact", UUID: "fact-1", Name: "WORKS_AT", Fact: "Bob works at TechCorp"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	p := graphiti.New(srv.URL)
	result, err := p.Ingest(context.Background(), graphiti.EpisodeInput{
		GroupID: "test-group",
		Content: "Bob works at TechCorp.",
	})
	require.NoError(t, err)
	require.Len(t, result.Entities, 1)
	assert.Equal(t, "Bob", result.Entities[0].Name)
	require.Len(t, result.Facts, 1)
	assert.Equal(t, "WORKS_AT", result.Facts[0].Name)
}

func TestIngest_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "extraction failed", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	p := graphiti.New(srv.URL)
	_, err := p.Ingest(context.Background(), graphiti.EpisodeInput{
		GroupID: "g", Content: "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestIngest_EmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(graphiti.IngestResult{})
	}))
	t.Cleanup(srv.Close)

	p := graphiti.New(srv.URL)
	result, err := p.Ingest(context.Background(), graphiti.EpisodeInput{
		GroupID: "g", Content: "",
	})
	require.NoError(t, err)
	assert.Empty(t, result.Entities)
	assert.Empty(t, result.Facts)
}
