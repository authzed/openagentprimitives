package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	scopetypes "github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/extracted_entity"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// debugTestToken is the token these tests configure the (now gated) debug
// route with. The route is served only when a token is set and presented; see
// healthState.handler.
const debugTestToken = "debug-test-token"

// getDebugAuthed issues an authorized GET against the gated debug route.
func getDebugAuthed(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+debugTestToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestDebugBindings_ReturnsMemoryDump(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	// Seed a SessionScope resource (replaces legacy binding entries).
	require.NoError(t, sessionscope.Put(approvedCtx(), mem, scope, scopetypes.Scope{
		Resources: []scopetypes.ScopeResource{{
			ResourceType: "github_repo",
			IDs:          []string{"authzed/spicedb"},
			Source:       scopetypes.SourceDefault,
		}},
		ScopeVersion: 1,
	}))
	require.NoError(t, extracted_entity.Record(approvedCtx(), mem, scope, extracted_entity.Content{
		ResourceType: "github_repo", ResourceID: "authzed/spicedb", TurnIndex: 1,
	}))
	require.NoError(t, extraction_state.Record(approvedCtx(), mem, scope, extraction_state.Content{
		TurnIndex: 1, Status: extraction_state.StatusComplete, CandidateCount: 1,
	}))

	hs := &healthState{debugToken: debugTestToken}
	hs.deps.Memory = approvedMem{mem}
	srv := httptest.NewServer(hs.handler())
	defer srv.Close()

	resp := getDebugAuthed(t, srv.URL+"/debug/sessions/ns/a/bindings")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var payload map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	assert.NotEmpty(t, payload["bindings"])
	assert.NotEmpty(t, payload["extracted"])
	assert.NotEmpty(t, payload["extraction_state"])
}

func TestDebugBindings_NotFoundForUnknownPath(t *testing.T) {
	hs := &healthState{debugToken: debugTestToken}
	hs.deps.Memory = memory.NewLocal(inmem.NewBackend())
	srv := httptest.NewServer(hs.handler())
	defer srv.Close()

	resp := getDebugAuthed(t, srv.URL+"/debug/sessions/ns/a/not-a-thing")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDebugBindings_EmptySession(t *testing.T) {
	hs := &healthState{debugToken: debugTestToken}
	hs.deps.Memory = memory.NewLocal(inmem.NewBackend())
	srv := httptest.NewServer(hs.handler())
	defer srv.Close()

	resp := getDebugAuthed(t, srv.URL+"/debug/sessions/ns/empty/bindings")
	defer resp.Body.Close()
	// Empty session returns 200 with empty arrays — operators expect a 200 + visible empty state, not 404.
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
