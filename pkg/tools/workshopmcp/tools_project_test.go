package workshopmcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectAgentToolArgs is this test file's own copy of the tool-call
// argument shape, used only to build the MCP request payload — the same
// role newTestWorkshopServer's siblings play in tools_thread_test.go.
type projectAgentToolArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// TestHandleProjectAgent_ReturnsProjectedName is the happy path: the
// operator's {"name": "..."} body decodes into the tool's success result,
// and the request the sidecar sent carries the bearer, method, path, and
// JSON body the operator route expects.
func TestHandleProjectAgent_ReturnsProjectedName(t *testing.T) {
	var gotAuth, gotMethod, gotPath, gotContentType string
	var gotBody projectAgentToolArgs
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"greeter-agent"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "the-operator-bearer")

	s := &Server{}
	res := callTool(t, s.handleProjectAgent, projectAgentToolArgs{Namespace: "b", Name: "greeter-agent"})
	require.False(t, res.IsError, "a successful projection must not be a tool error")

	assert.Equal(t, "Bearer the-operator-bearer", gotAuth, "bearer sent from the token env var")
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/workshop/project-agent", gotPath)
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, "b", gotBody.Namespace)
	assert.Equal(t, "greeter-agent", gotBody.Name)

	body := decodeResultBody(t, res)
	assert.Equal(t, "greeter-agent", body["name"])
	assert.Equal(t, "projected", body["status"])
}

// TestHandleProjectAgent_NotReachable_SurfacesAsToolError is the
// not-reachable refusal (Ruling A): the operator's 403 — the source class
// never appeared in this builder's own agents-in-thread result — must
// surface as a tool error naming why, never a silent success that would
// leave the model believing a stand-in was created when it was not.
func TestHandleProjectAgent_NotReachable_SurfacesAsToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "class other-ns/some-agent is not reachable from this builder's own conversation thread; refusing to project a stand-in for it", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	res := callTool(t, s.handleProjectAgent, projectAgentToolArgs{Namespace: "other-ns", Name: "some-agent"})
	require.True(t, res.IsError, "a not-reachable refusal must surface as a tool error, never a silent success")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "not reachable from this builder's own conversation thread")
	assert.Contains(t, body["error"], "403")
}

// TestHandleProjectAgent_NameCollision_SurfacesAsToolError is the
// name-collision refusal (Ruling D): the operator's 409 — the bare stand-in
// name already names an AgentClass this workshop's builder authored itself
// — must surface as a tool error naming the collision, never be swallowed
// or mistaken for success.
func TestHandleProjectAgent_NameCollision_SurfacesAsToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "ws-1/greeter-agent already names an AgentClass this workshop's builder authored — refusing to overwrite it with a stand-in", http.StatusConflict)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	res := callTool(t, s.handleProjectAgent, projectAgentToolArgs{Namespace: "b", Name: "greeter-agent"})
	require.True(t, res.IsError, "a name collision must surface as a tool error, never a silent success")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "already names an AgentClass this workshop's builder authored")
	assert.Contains(t, body["error"], "409")
}

// TestHandleProjectAgent_MalformedBody_SurfacesAsToolError proves a
// non-JSON 200 body is surfaced as a tool error rather than silently
// decoding to a zero-value (and therefore falsely "successful") result.
func TestHandleProjectAgent_MalformedBody_SurfacesAsToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	res := callTool(t, s.handleProjectAgent, projectAgentToolArgs{Namespace: "b", Name: "greeter-agent"})
	require.True(t, res.IsError, "a malformed body must surface as a tool error, not a false success")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "decode response")
}

// TestHandleProjectAgent_EmptyNameField_SurfacesAsToolError proves a 200
// response whose body decodes cleanly but names no stand-in (an empty
// "name" field) is still refused rather than reported as success — the
// same "a refusal must never decode as a success" discipline, for the case
// where the JSON itself is well-formed but empty.
func TestHandleProjectAgent_EmptyNameField_SurfacesAsToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":""}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "tok")

	s := &Server{}
	res := callTool(t, s.handleProjectAgent, projectAgentToolArgs{Namespace: "b", Name: "greeter-agent"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "named no stand-in")
}

// TestHandleProjectAgent_MissingOperatorEnv_SurfacesAsToolError is the
// fail-closed branch: an unwired OPERATOR_MEMORY_URL/token must refuse
// rather than proceed with an empty bearer or an unset target.
func TestHandleProjectAgent_MissingOperatorEnv_SurfacesAsToolError(t *testing.T) {
	t.Setenv("OPERATOR_MEMORY_URL", "")
	t.Setenv("token", "")

	s := &Server{}
	res := callTool(t, s.handleProjectAgent, projectAgentToolArgs{Namespace: "b", Name: "greeter-agent"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "OPERATOR_MEMORY_URL")
}

// TestHandleProjectAgent_MissingArguments_SurfacesAsToolError proves the
// tool refuses locally, before ever reaching the operator, when the model
// omits namespace or name.
func TestHandleProjectAgent_MissingArguments_SurfacesAsToolError(t *testing.T) {
	t.Setenv("OPERATOR_MEMORY_URL", "http://unused.invalid")
	t.Setenv("token", "tok")

	s := &Server{}
	res := callTool(t, s.handleProjectAgent, projectAgentToolArgs{Namespace: "", Name: "greeter-agent"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "namespace and name are both required")
}
