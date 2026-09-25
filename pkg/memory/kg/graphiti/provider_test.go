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
	kggraphiti "github.com/authzed/openagentprimitives/pkg/memory/kg/graphiti"
	searchgraphiti "github.com/authzed/openagentprimitives/pkg/memory/search/graphiti"
)

// testEntityUUID is the entity id every test in this package names. It has to
// be UUID-shaped: GetEntity puts it in a URL path, so it validates the shape
// and refuses anything else before issuing a request. A short opaque fixture
// ("ent-1") would make the group-filtering tests below pass on the WRONG
// error — refused for its shape, never reaching the group check they exist to
// exercise.
const testEntityUUID = "6c3e8f2a-1b4d-4f7e-9a3c-2d5e8b1f0a67"

func TestIngest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/messages", r.URL.Path)
		var req struct {
			GroupID  string `json:"group_id"`
			Messages []struct {
				Content  string `json:"content"`
				RoleType string `json:"role_type"`
			} `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "session-1", req.GroupID)
		assert.Equal(t, "Hello Bob", req.Messages[0].Content)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{})
	}))
	t.Cleanup(srv.Close)

	sp := searchgraphiti.New(srv.URL)
	p := kggraphiti.New(sp)
	err := p.Ingest(context.Background(), memory.KGInput{
		GroupID: "session-1", Content: "Hello Bob", Role: "user",
	})
	require.NoError(t, err)
}

func TestSearchFacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/search", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"facts": []map[string]interface{}{
				{"uuid": "f1", "name": "WORKS_AT", "fact": "Bob works at TechCorp"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	sp := searchgraphiti.New(srv.URL)
	p := kggraphiti.New(sp)
	facts, err := p.SearchFacts(context.Background(), "Bob", 10)
	require.NoError(t, err)
	require.Len(t, facts, 1)
	assert.Equal(t, "WORKS_AT", facts[0].Name)
	assert.Equal(t, "Bob works at TechCorp", facts[0].Fact)
}

func TestGetEntity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/entity-edge/"+testEntityUUID, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"uuid": testEntityUUID, "name": "Bob", "summary": "Engineer",
		})
	}))
	t.Cleanup(srv.Close)

	sp := searchgraphiti.New(srv.URL)
	p := kggraphiti.New(sp)
	ent, err := p.GetEntity(context.Background(), testEntityUUID)
	require.NoError(t, err)
	assert.Equal(t, "Bob", ent.Name)
}

// TestCommunities_ReturnsUnsupportedWithoutCallingTheServer is the regression
// test for calling Communities against a live Graphiti: the real /get-memory
// response (graph_service/dto/retrieve.py's GetMemoryResponse) never carries a
// "communities" key at all — the real handler's own source
// (graph_service/routers/retrieve.py) only ever builds a facts search, so no
// request shape can make this endpoint answer the question. Round-tripping a
// request that can never satisfy the caller is worse than refusing up front:
// it also 422s (missing center_node_uuid/messages), which is the error this
// whole fix started from.
func TestCommunities_ReturnsUnsupportedWithoutCallingTheServer(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sp := searchgraphiti.New(srv.URL)
	p := kggraphiti.New(sp)
	comms, err := p.Communities(context.Background(), "group-1")

	require.ErrorIs(t, err, memory.ErrKGUnsupported)
	assert.Nil(t, comms)
	assert.False(t, called, "Communities must not round-trip a request it knows will never be answered")
}
