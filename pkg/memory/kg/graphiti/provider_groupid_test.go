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

// recordingServer spins up an httptest.Server that decodes each request body
// as a generic map and stashes it for assertion, keyed by URL path (last
// request per path wins, which is all these tests need).
func recordingServer(t *testing.T) (*httptest.Server, map[string]map[string]interface{}) {
	t.Helper()
	bodies := make(map[string]map[string]interface{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		bodies[r.URL.Path] = body
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{})
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

func TestSearchFactsGroupID_ScopedAppliesGroupIDsFilter(t *testing.T) {
	srv, bodies := recordingServer(t)
	sp := searchgraphiti.New(srv.URL)
	p := kggraphiti.New(sp)

	scoped := memory.WithKGScope(context.Background(), memory.Scope{Kind: "session", ID: "nsA/sessA"})
	_, err := p.SearchFacts(scoped, "q", 5)
	require.NoError(t, err)

	body := bodies["/search"]
	require.NotNil(t, body)
	groupIDs, ok := body["group_ids"].([]interface{})
	require.True(t, ok, "expected group_ids in body, got %+v", body)
	require.Len(t, groupIDs, 1)
	assert.Equal(t, "nsA/sessA", groupIDs[0])
}

func TestSearchFactsGroupID_UnscopedSendsNoGroupFilter(t *testing.T) {
	srv, bodies := recordingServer(t)
	sp := searchgraphiti.New(srv.URL)
	p := kggraphiti.New(sp)

	_, err := p.SearchFacts(context.Background(), "q", 5)
	require.NoError(t, err)

	body := bodies["/search"]
	require.NotNil(t, body)
	_, present := body["group_ids"]
	assert.False(t, present, "unscoped call must not send a group filter, got %+v", body)
}

func TestEntityFactsGroupID_ScopedAppliesGroupID(t *testing.T) {
	srv, bodies := recordingServer(t)
	sp := searchgraphiti.New(srv.URL)
	p := kggraphiti.New(sp)

	scoped := memory.WithKGScope(context.Background(), memory.Scope{Kind: "session", ID: "nsA/sessA"})
	_, err := p.EntityFacts(scoped, "uuid-1")
	require.NoError(t, err)

	body := bodies["/get-memory"]
	require.NotNil(t, body)
	assert.Equal(t, "nsA/sessA", body["group_id"])
}

func TestEntityFactsGroupID_UnscopedSendsEmptyGroupID(t *testing.T) {
	srv, bodies := recordingServer(t)
	sp := searchgraphiti.New(srv.URL)
	p := kggraphiti.New(sp)

	_, err := p.EntityFacts(context.Background(), "uuid-1")
	require.NoError(t, err)

	body := bodies["/get-memory"]
	require.NotNil(t, body)
	assert.Equal(t, "", body["group_id"])
}

// entityServer answers /entity-edge/{uuid} with a fixed body.
func entityServer(t *testing.T, body map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGetEntity_ScopedReadIsRefusedWithoutCallingTheServer is the regression
// test for the false-positive this scope check used to produce against a real
// server. The check used to compare a RESPONSE group_id against the caller's
// scope, but /entity-edge/{uuid}'s real FactResult (graph_service/dto/
// retrieve.py upstream) never carries a group_id at all — get_fact_result_from_
// edge never sets one. So the old comparison always saw "" and refused EVERY
// scoped caller, not just a foreign one: a security fix that, against the real
// deployment, silently broke the legitimate case it meant to keep working. A
// field that is never on the wire cannot prove OR disprove scope, so a scoped
// read is refused before the request is even made, rather than guessed at from
// content that was never going to be there.
func TestGetEntity_ScopedReadIsRefusedWithoutCallingTheServer(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"uuid": testEntityUUID, "name": "Bob"})
	}))
	t.Cleanup(srv.Close)
	p := kggraphiti.New(searchgraphiti.New(srv.URL))

	scoped := memory.WithKGScope(context.Background(), memory.Scope{Kind: "session", ID: "nsA/sessA"})
	ent, err := p.GetEntity(scoped, testEntityUUID)

	require.ErrorIs(t, err, memory.ErrKGUnsupported)
	assert.Nil(t, ent)
	assert.False(t, called, "a read that will be refused regardless of content must not be made")
}

// The platform-admin browser deliberately sets no KG scope and stays
// cross-session; it is gated on platform#view_audit instead.
func TestGetEntityGroupID_UnscopedReadIsUnfiltered(t *testing.T) {
	srv := entityServer(t, map[string]interface{}{
		"uuid": testEntityUUID, "name": "Bob", "group_id": "nsB/sessB",
	})
	p := kggraphiti.New(searchgraphiti.New(srv.URL))

	ent, err := p.GetEntity(context.Background(), testEntityUUID)

	require.NoError(t, err)
	require.NotNil(t, ent)
	assert.Equal(t, "Bob", ent.Name)
}

// TestRelatedEntities_ReturnsUnsupportedWithoutCallingTheServer is the
// regression test for calling RelatedEntities against a live Graphiti: the
// real /get-memory response never carries an "entities" key (its
// GetMemoryResponse is facts-only — see graph_service/dto/retrieve.py
// upstream), for a scoped OR unscoped caller alike, so no request this client
// could send would ever get real data back.
func TestRelatedEntities_ReturnsUnsupportedWithoutCallingTheServer(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p := kggraphiti.New(searchgraphiti.New(srv.URL))

	scoped := memory.WithKGScope(context.Background(), memory.Scope{Kind: "session", ID: "nsA/sessA"})
	ents, err := p.RelatedEntities(scoped, "uuid-1", 5)
	require.ErrorIs(t, err, memory.ErrKGUnsupported)
	assert.Nil(t, ents)

	ents, err = p.RelatedEntities(context.Background(), "uuid-1", 5)
	require.ErrorIs(t, err, memory.ErrKGUnsupported)
	assert.Nil(t, ents)

	assert.False(t, called, "RelatedEntities must not round-trip a request it knows will never be answered")
}

// TestEntityFactsBody_IncludesMessagesField is the regression test for the
// 422 EntityFacts produces against a real Graphiti: GetMemoryRequest requires
// group_id, center_node_uuid, AND messages (graph_service/dto/retrieve.py
// upstream) — a body missing any of the three is refused before the search
// ever runs.
func TestEntityFactsBody_IncludesMessagesField(t *testing.T) {
	srv, bodies := recordingServer(t)
	p := kggraphiti.New(searchgraphiti.New(srv.URL))

	_, err := p.EntityFacts(context.Background(), "uuid-1")
	require.NoError(t, err)

	body := bodies["/get-memory"]
	require.NotNil(t, body)
	_, present := body["messages"]
	assert.True(t, present, "messages is a required field on GetMemoryRequest; omitting it 422s against the real server")
}

// getMemoryServer answers /get-memory with a fixed JSON body (facts+entities),
// modelling a Graphiti that TAGS each item with its group_id.
func getMemoryServer(t *testing.T, body map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// center_node_uuid is model-supplied and /get-memory's group_id is a
// request-side filter Graphiti is trusted to honor. Belt-and-suspenders, the
// provider drops any RETURNED fact/entity whose group_id is present and does
// not match this scope — mirroring GetEntity — so a Graphiti that leaks a
// foreign-group item on a model-chosen center node cannot cross sessions.
func TestEntityFactsGroupID_DropsAForeignGroupFactInTheResponse(t *testing.T) {
	srv := getMemoryServer(t, map[string]interface{}{
		"facts": []map[string]interface{}{
			{"uuid": "f-mine", "name": "r", "fact": "in scope", "group_id": "nsA/sessA"},
			{"uuid": "f-foreign", "name": "r", "fact": "SECRET", "group_id": "nsB/sessB"},
			{"uuid": "f-untagged", "name": "r", "fact": "no tag"},
		},
	})
	p := kggraphiti.New(searchgraphiti.New(srv.URL))
	scoped := memory.WithKGScope(context.Background(), memory.Scope{Kind: "session", ID: "nsA/sessA"})

	facts, err := p.EntityFacts(scoped, testEntityUUID)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, f := range facts {
		got[f.UUID] = true
	}
	assert.True(t, got["f-mine"], "an in-scope fact is returned")
	assert.True(t, got["f-untagged"], "an untagged fact is unaffected (primary isolation is the request filter)")
	assert.False(t, got["f-foreign"], "a fact tagged with another group must be dropped")
}
