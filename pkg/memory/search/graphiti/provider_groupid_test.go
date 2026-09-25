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
	searchgraphiti "github.com/authzed/openagentprimitives/pkg/memory/search/graphiti"
)

func TestSearchGroupID_ScopesRequestByGroupIDs(t *testing.T) {
	var body map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"facts": []map[string]interface{}{}})
	}))
	t.Cleanup(srv.Close)

	p := searchgraphiti.New(srv.URL)
	_, err := p.Search(context.Background(), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "nsA/sessA"}},
		Text:   "q",
		Limit:  5,
	})
	require.NoError(t, err)

	require.NotNil(t, body)
	groupIDs, ok := body["group_ids"].([]interface{})
	require.True(t, ok, "expected group_ids in body, got %+v", body)
	require.Len(t, groupIDs, 1)
	assert.Equal(t, "nsA/sessA", groupIDs[0])
}

func TestSearchGroupID_UnscopedSendsNoGroupFilter(t *testing.T) {
	var body map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"facts": []map[string]interface{}{}})
	}))
	t.Cleanup(srv.Close)

	p := searchgraphiti.New(srv.URL)
	_, err := p.Search(context.Background(), memory.SearchRequest{
		Text:  "q",
		Limit: 5,
	})
	require.NoError(t, err)

	require.NotNil(t, body)
	_, present := body["group_ids"]
	assert.False(t, present, "unscoped search must not send a group filter, got %+v", body)
}
