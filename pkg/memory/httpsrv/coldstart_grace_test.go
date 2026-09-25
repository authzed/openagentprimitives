// pkg/memory/httpsrv/coldstart_grace_test.go — the cold-start grace on the
// per-session bearer path.
//
// The operator's in-process token registry is empty on every process boot and
// the AgentSession reconciler refills it per session, so a live runner writing
// in that window would take a terminal 401 the instant the operator restarts.
// WithColdRegistryGrace turns that 401 into a retryable 503 for a bounded window
// after startup; these tests pin when it fires, when it does not, and that a
// registered bearer is never caught by it.
package httpsrv_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register label/... Kinds
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// TestColdRegistryGrace_UnregisteredBearer pins the refusal a per-session bearer
// gets when NO session is registered under it: a retryable 503 inside the grace
// window, a permanent 401 outside it (or when the grace is disabled). Only the
// 503 case depends on the new branch — the 401 cases are the fall-through and
// hold with or without it, which is what makes the 503 row the one that proves
// the branch is reached.
func TestColdRegistryGrace_UnregisteredBearer(t *testing.T) {
	cases := []struct {
		name       string
		grace      time.Duration
		wantStatus int
		wantRetry  bool // Retry-After header present
	}{
		{
			name:       "within grace: 503 retryable with Retry-After",
			grace:      time.Hour, // handler was just built, so uptime << grace
			wantStatus: http.StatusServiceUnavailable,
			wantRetry:  true,
		},
		{
			name:       "grace disabled (0): plain 401",
			grace:      0,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "past grace: plain 401",
			grace:      time.Nanosecond, // elapses before the request lands
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			reg := tokens.NewRegistry() // deliberately empty: no session registered
			srv := httptest.NewServer(httpsrv.NewHandler(mem, reg,
				httpsrv.WithColdRegistryGrace(tc.grace)))
			t.Cleanup(srv.Close)

			// A per-session read route; the bearer "ghost-tok" matches no
			// registration, so ServeHTTP refuses it before touching the facade.
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/label/ns/n", nil)
			require.NoError(t, err, "NewRequest")
			resp := do(t, authed(req, "ghost-tok"))
			defer resp.Body.Close()

			assert.Equal(t, tc.wantStatus, resp.StatusCode, "status for grace=%s", tc.grace)
			if tc.wantRetry {
				assert.NotEmpty(t, resp.Header.Get("Retry-After"),
					"a warming 503 must carry Retry-After so a caller can pace its retry")
			}
		})
	}
}

// TestColdRegistryGrace_RegisteredBearerUnaffected proves the grace fires only on
// the registry MISS: a bearer the operator DOES know reads its own session
// normally even with a wide-open grace window, so warming never 503s a valid
// read/write.
func TestColdRegistryGrace_RegisteredBearerUnaffected(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "sess-tok", "")
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg,
		httpsrv.WithColdRegistryGrace(time.Hour)))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/label/ns/n", nil)
	require.NoError(t, err, "NewRequest")
	resp := do(t, authed(req, "sess-tok"))
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"a registered bearer must read normally, never caught by the cold-start grace")
}
