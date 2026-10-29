// pkg/memory/httpsrv/registry_restore_test.go — the per-session bearer path
// while the token registry is being restored after an operator restart.
//
// The operator's in-process token registry is empty on every process boot, and
// the operator restores every live session's token in one pass once its caches
// have synced. Until that pass finishes, an unknown bearer is answered with a
// retryable 503 instead of the terminal 401 that would fail a live session;
// these tests pin when that happens, when it does not, and that a registered
// bearer is never caught by it.
package httpsrv_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register label/... Kinds
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// TestRegistryRestore_UnregisteredBearer pins the refusal a per-session bearer
// gets when NO session is registered under it: a retryable 503 while the
// registry is restoring, a permanent 401 once it has been restored or when it
// was never put in the restoring state. Only the 503 row depends on the
// restoring branch; the 401 rows are the fall-through, which is what makes the
// 503 row the one that proves the branch is reached.
func TestRegistryRestore_UnregisteredBearer(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(*tokens.Registry)
		wantStatus int
		wantRetry  bool // Retry-After header present
	}{
		{
			name:       "restoring: 503 retryable with Retry-After",
			setup:      func(r *tokens.Registry) { r.BeginRestore() },
			wantStatus: http.StatusServiceUnavailable,
			wantRetry:  true,
		},
		{
			name:       "restore finished: plain 401",
			setup:      func(r *tokens.Registry) { r.BeginRestore(); r.FinishRestore() },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "never restoring (in-process harness): plain 401",
			setup:      func(*tokens.Registry) {},
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			reg := tokens.NewRegistry() // deliberately empty: no session registered
			tc.setup(reg)
			srv := httptest.NewServer(httpsrv.NewHandler(mem, reg))
			t.Cleanup(srv.Close)

			// A per-session read route; the bearer "ghost-tok" matches no
			// registration, so ServeHTTP refuses it before touching the facade.
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/label/ns/n", nil)
			require.NoError(t, err, "NewRequest")
			resp := do(t, authed(req, "ghost-tok"))
			defer resp.Body.Close()

			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			if tc.wantRetry {
				assert.NotEmpty(t, resp.Header.Get("Retry-After"),
					"a warming 503 must carry Retry-After so a caller can pace its retry")
			}
		})
	}
}

// TestRegistryRestore_RegisteredBearerUnaffected proves the restoring state
// fires only on the registry MISS: a bearer the operator DOES know reads its
// own session normally mid-restore, so a restore never 503s a valid request.
func TestRegistryRestore_RegisteredBearerUnaffected(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.BeginRestore()
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "sess-tok", "")
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/label/ns/n", nil)
	require.NoError(t, err, "NewRequest")
	resp := do(t, authed(req, "sess-tok"))
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"a registered bearer must read normally, never caught by the restoring state")
}
