package workshopmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestToolResultsOnly_DropsProseKeepsToolBlocks is the pure-filter contract:
// a builder session must be able to see what a child DID (tool calls and
// their results, errors included) without ever seeing what it SAID (prose,
// attachments).
func TestToolResultsOnly_DropsProseKeepsToolBlocks(t *testing.T) {
	turns := []memory.Turn{{Role: "assistant", Content: []memory.ContentBlock{
		{Type: "text", Text: "let me look that up"}, // prose → dropped
		{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "u1", Name: "get_weather", Input: []byte(`{"city":"X"}`)}},
		{Type: "tool_result", ToolResult: &memory.ToolResultBlock{ToolUseID: "u1", Content: "sunny"}},
		{Type: "tool_result", ToolResult: &memory.ToolResultBlock{ToolUseID: "u2", Content: "boom", IsError: true}}, // errors kept
		{Type: "attachment"}, // dropped
	}}}
	got := toolResultsOnly(turns)
	require.Len(t, got, 3, "two tool blocks + one error result; prose and attachment dropped")
	assert.Equal(t, "call", got[0].Kind)
	assert.Equal(t, "get_weather", got[0].Tool)
	assert.Equal(t, "result", got[1].Kind)
	assert.Equal(t, "sunny", got[1].Result)
	assert.True(t, got[2].IsError)
	for _, r := range got {
		assert.NotContains(t, r.Result, "let me look that up", "no prose may leak into a record")
	}
}

// TestReadChildToolResults_ReturnsFilteredRecords exercises the bearer GET
// client end to end over httptest: the response carries a mix of prose and
// tool blocks, and only the tool records must come back.
func TestReadChildToolResults_ReturnsFilteredRecords(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"turns": []memory.Turn{{Role: "assistant", Content: []memory.ContentBlock{
				{Type: "text", Text: "narrating what I'm about to do"},
				{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "u1", Name: "run_test", Input: []byte(`{"x":1}`)}},
				{Type: "tool_result", ToolResult: &memory.ToolResultBlock{ToolUseID: "u1", Content: "ok"}},
			}}},
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "the-operator-bearer")

	s := &Server{}
	got, err := s.readChildToolResults(context.Background(), "child-1")
	require.NoError(t, err)

	assert.Equal(t, "Bearer the-operator-bearer", gotAuth, "bearer sent from the token env var")
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/workshop/transcript/child-1", gotPath)

	require.Len(t, got.Records, 2, "prose dropped; call + result kept")
	assert.Equal(t, "call", got.Records[0].Kind)
	assert.Equal(t, "run_test", got.Records[0].Tool)
	assert.Equal(t, "result", got.Records[1].Kind)
	assert.Equal(t, "ok", got.Records[1].Result)
	for _, r := range got.Records {
		assert.NotContains(t, r.Result, "narrating", "no prose may leak into a record")
	}
}

// TestReadChildToolResults_TrimsTrailingSlashOnOperatorURL mirrors the
// export.go sibling's coverage of the same trailing-slash concatenation
// hazard on the bearer client.
func TestReadChildToolResults_TrimsTrailingSlashOnOperatorURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"turns": []memory.Turn{}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL+"/")
	t.Setenv("token", "tok")

	s := &Server{}
	_, err := s.readChildToolResults(context.Background(), "child-1")
	require.NoError(t, err)
	assert.Equal(t, "/workshop/transcript/child-1", gotPath, "no double slash even when OPERATOR_MEMORY_URL has a trailing one")
}

// TestReadChildToolResults_NonOKStatus_ReturnsError is the refusing-direction
// case: the operator's own denial (tuple false, workshop not Ready, a child
// outside this workshop, …) must surface as an error, never as an empty or
// successful result set — a caller that ignored the error could otherwise
// mistake "denied" for "no tool calls happened".
func TestReadChildToolResults_NonOKStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "session does not hold agentsession:ws-1/child-1#read_transcript", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	got, err := s.readChildToolResults(context.Background(), "child-1")
	require.Error(t, err)
	assert.Empty(t, got.Records, "a refusal must never read as 'no tool calls happened'")
	assert.Empty(t, got.Audit)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), "read_transcript")
}

// TestReadChildToolResults_MalformedResponseBody_ReturnsError proves a
// non-JSON 200 body is surfaced as an error, not decoded into a zero-value
// (and therefore silently empty) result set.
func TestReadChildToolResults_MalformedResponseBody_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	_, err := s.readChildToolResults(context.Background(), "child-1")
	require.Error(t, err)
}

// TestReadChildToolResults_MissingOperatorEnv_ReturnsError is the fail-closed
// branch: with OPERATOR_MEMORY_URL or token unset, the client must refuse
// rather than proceed with an empty bearer or an unset target.
func TestReadChildToolResults_MissingOperatorEnv_ReturnsError(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		token string
	}{
		{"missing OPERATOR_MEMORY_URL", "", "tok"},
		{"missing token", "http://127.0.0.1:1", ""},
		{"missing both", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPERATOR_MEMORY_URL", tc.url)
			t.Setenv("token", tc.token)
			s := &Server{}
			got, err := s.readChildToolResults(context.Background(), "child-1")
			require.Error(t, err)
			assert.Empty(t, got.Records)
			assert.Empty(t, got.Audit)
		})
	}
}
