package workshopmcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleAgentsInThread_ReturnsDecodedAgents is the happy path: the
// operator's array body decodes into the tool's success result, one entry
// per participant/descendant, field for field.
func TestHandleAgentsInThread_ReturnsDecodedAgents(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"namespace":"b","name":"other-agent","class":"greeter-agent","role":"participant"},
			{"namespace":"b","name":"other-agent-child","class":"helper-agent","role":"descendant"}
		]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "the-operator-bearer")

	s := &Server{}
	res := callTool(t, s.handleAgentsInThread, struct{}{})
	require.False(t, res.IsError, "a successful lookup must not be a tool error")

	assert.Equal(t, "Bearer the-operator-bearer", gotAuth, "bearer sent from the token env var")
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/workshop/agents-in-thread", gotPath, "no path parameter — the route derives the thread from the bearer alone")

	body := decodeResultBody(t, res)
	agents, ok := body["agents"].([]any)
	require.True(t, ok, "agents must be a list")
	require.Len(t, agents, 2)

	participant := agents[0].(map[string]any)
	assert.Equal(t, "b", participant["namespace"])
	assert.Equal(t, "other-agent", participant["name"])
	assert.Equal(t, "greeter-agent", participant["class"])
	assert.Equal(t, "participant", participant["role"])

	descendant := agents[1].(map[string]any)
	assert.Equal(t, "other-agent-child", descendant["name"])
	assert.Equal(t, "descendant", descendant["role"])
}

// TestHandleAgentsInThread_EmptyThread_ReturnsEmptyList proves a
// genuinely-empty operator response ("nobody else is on this thread")
// decodes to an empty list rather than being mistaken for a failure —
// the negative case (a real failure) must NOT look like this one, which is
// exactly what the next two tests pin.
func TestHandleAgentsInThread_EmptyThread_ReturnsEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	res := callTool(t, s.handleAgentsInThread, struct{}{})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Empty(t, body["agents"])
}

// TestHandleAgentsInThread_NonOKStatus_SurfacesAsToolError is the
// fail-loud case for a denied or refused lookup (unauthorized bearer,
// workshop not Ready, tuple false, …): the caller must see a tool error
// naming the reason, never a silent empty list, which a model would read as
// "nobody else worked this thread" — the worst possible misreading of a
// refusal.
func TestHandleAgentsInThread_NonOKStatus_SurfacesAsToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "workshop ws-1 is not Ready", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	res := callTool(t, s.handleAgentsInThread, struct{}{})
	require.True(t, res.IsError, "a non-200 from the route must surface as a tool error, not a silent empty list")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "workshop ws-1 is not Ready")
	assert.Contains(t, body["error"], "403")
}

// TestHandleAgentsInThread_MalformedBody_SurfacesAsToolError proves a
// non-JSON 200 body is surfaced as a tool error rather than silently
// decoding to a zero-value (and therefore empty) agent list.
func TestHandleAgentsInThread_MalformedBody_SurfacesAsToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	res := callTool(t, s.handleAgentsInThread, struct{}{})
	require.True(t, res.IsError, "a malformed body must surface as a tool error, not an empty result")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "decode response")
}

// TestHandleAgentsInThread_MissingOperatorEnv_SurfacesAsToolError is the
// fail-closed branch: an unwired OPERATOR_MEMORY_URL/token must refuse
// rather than proceed with an empty bearer or an unset target.
func TestHandleAgentsInThread_MissingOperatorEnv_SurfacesAsToolError(t *testing.T) {
	t.Setenv("OPERATOR_MEMORY_URL", "")
	t.Setenv("token", "")

	s := &Server{}
	res := callTool(t, s.handleAgentsInThread, struct{}{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "OPERATOR_MEMORY_URL")
}
