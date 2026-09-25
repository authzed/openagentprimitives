package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	scopetypes "github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// debugStateWithToken builds a healthState holding one session's scope, with
// the debug route gated on token.
func debugStateWithToken(t *testing.T, token string) *healthState {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	require.NoError(t, sessionscope.Put(approvedCtx(), mem,
		memory.Scope{Kind: "session", ID: "victim-ns/victim"},
		scopetypes.Scope{Resources: []scopetypes.ScopeResource{{
			ResourceType: "github_repo", IDs: []string{"acme/secret"}, Source: scopetypes.SourceDefault,
		}}}))

	hs := &healthState{}
	hs.deps.Memory = mem
	hs.debugToken = token
	return hs
}

func getDebug(t *testing.T, h http.Handler, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/debug/sessions/victim-ns/victim/bindings", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The debug route shares the probe mux, which is served on :8080 across all
// interfaces with no TLS and no auth, and it reads through authzd's memory
// token — which the operator's memory server treats as cluster-wide. So it
// dumped ANY session's scope and the span of text a candidate was extracted
// from, to any caller that could reach the pod.
//
// NetworkPolicy is the only thing that stopped it, and nothing verifies the
// CNI enforces NetworkPolicy: stock EKS without the policy add-on and GKE
// Standard without Dataplane V2 do not, and on those every pod in the cluster
// — a tenant's sandbox included — could read it.
func TestDebugBindings_RefusesAnUnauthenticatedCaller(t *testing.T) {
	hs := debugStateWithToken(t, "s3cret")

	rec := getDebug(t, hs.handler(), "")

	assert.Equal(t, http.StatusNotFound, rec.Code,
		"an unauthenticated caller must not reach another session's scope")
	assert.NotContains(t, rec.Body.String(), "victim-ns/victim",
		"the refusal must not confirm the session it is refusing to describe")
}

func TestDebugBindings_RefusesAWrongToken(t *testing.T) {
	hs := debugStateWithToken(t, "s3cret")

	rec := getDebug(t, hs.handler(), "not-the-token")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "victim-ns/victim")
}

// An operator holding the component token keeps the affordance.
func TestDebugBindings_AllowsTheComponentToken(t *testing.T) {
	hs := debugStateWithToken(t, "s3cret")

	rec := getDebug(t, hs.handler(), "s3cret")

	require.Equal(t, http.StatusOK, rec.Code, "the component token must still reach the route")
	// The handler ran: it names the scope it was asked about. What it can
	// READ depends on the approval on the request context, which is the
	// caller's business and not what this test is about.
	assert.Contains(t, rec.Body.String(), "victim-ns/victim")
}

// With no token configured the route is not served at all. A debug affordance
// whose gate is unset must not fall open — that is how it came to be reachable
// in the first place.
func TestDebugBindings_IsNotServedWithoutAConfiguredToken(t *testing.T) {
	hs := debugStateWithToken(t, "")

	rec := getDebug(t, hs.handler(), "")

	assert.Equal(t, http.StatusNotFound, rec.Code,
		"no configured token means no debug route, not an open one")
}

// The probes must stay reachable without credentials — kubelet does not send
// one, and a liveness probe that started 401ing would crash-loop the pod.
func TestProbesRemainUnauthenticated(t *testing.T) {
	hs := debugStateWithToken(t, "s3cret")
	hs.natsConnected.Store(true)
	hs.subActive.Store(true)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		hs.handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "%s must not require a credential", path)
	}
}
