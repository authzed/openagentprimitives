// pkg/memory/httpsrv/handlekg_door_test.go
//
// Door test for the last enforcement gate on the KG read path: handleKG must
// refuse a request whose context carries no ReadKG approval for the scope,
// BEFORE dispatching to the KGQuerier. This file is package httpsrv (not
// httpsrv_test) so the missing-approval case can call the unexported
// handleKG directly, bypassing ServeHTTP's own minting — every request that
// reaches handleKG via the real HTTP entry point is already minted a
// ReadKG (or system) approval, so that path alone can never exercise the
// door's deny branch.
package httpsrv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/sentinel"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// fakeKGQuerier is a minimal KGQuerier that records how many times it was
// invoked and the KG scope threaded via ctx, so tests can assert the door
// blocked dispatch (calls == 0) or that the correct scope reached the
// provider on the allow path.
type fakeKGQuerier struct {
	calls     int
	lastScope memory.Scope
}

func (f *fakeKGQuerier) SearchFacts(ctx context.Context, _ string, _ int) ([]memory.KGFact, error) {
	f.calls++
	if s, ok := memory.KGScopeFrom(ctx); ok {
		f.lastScope = s
	}
	return []memory.KGFact{{Fact: "stub"}}, nil
}

func (f *fakeKGQuerier) GetEntity(_ context.Context, _ string) (*memory.KGEntity, error) {
	f.calls++
	return nil, nil
}

func (f *fakeKGQuerier) EntityFacts(_ context.Context, _ string) ([]memory.KGFact, error) {
	f.calls++
	return nil, nil
}

func (f *fakeKGQuerier) RelatedEntities(_ context.Context, _ string, _ int) ([]memory.KGEntity, error) {
	f.calls++
	return nil, nil
}

func (f *fakeKGQuerier) Communities(_ context.Context, _ string) ([]memory.KGCommunity, error) {
	f.calls++
	return nil, nil
}

// TestHandleKGDoor_MissingApproval_403 calls handleKG directly with a ctx
// carrying no approval at all. The door must refuse before dispatching to
// the KGQuerier — this is the unit-level proof that handleKG itself, not
// just the token-mint path in ServeHTTP, enforces ReadKG.
func TestHandleKGDoor_MissingApproval_403(t *testing.T) {
	kg := &fakeKGQuerier{}
	h := &handler{kg: kg}

	req := httptest.NewRequest(http.MethodGet, "/memory/_kg/nsA/sessA?action=search&q=hi", nil)
	req = req.WithContext(context.Background()) // no approval attached
	w := httptest.NewRecorder()

	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	h.handleKG(w, req, scope)

	assert.Equal(t, http.StatusForbidden, w.Code, "handleKG without a ReadKG approval must 403")
	assert.Equal(t, 0, kg.calls, "the KGQuerier must not be invoked when approval is missing")
	// This door refuses for exactly the reason the facade's ErrMissingApproval
	// names, so it must be identified as such on the wire. Otherwise one refusal
	// arrives at the caller as a sentinel and the other as an opaque 403,
	// depending only on which layer caught it first.
	assert.Equal(t, "missing_approval", w.Header().Get(sentinel.Header),
		"the door's refusal must carry the same discriminator the facade's does")
}

// TestHandleKGDoor_CrossSessionToken_403 exercises the full ServeHTTP path: a
// per-session token scoped ONLY to nsB/sessB must not reach nsA/sessA's _kg
// route. This is the existing Authorizes(token, urlSess) check (~httpsrv.go:227)
// that runs before any approval is minted — asserted here as a door-adjacent
// regression guard, since it's the first line of defense the new gate backs up.
func TestHandleKGDoor_CrossSessionToken_403(t *testing.T) {
	kg := &fakeKGQuerier{}
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "nsB", Name: "sessB"}, "tok-b", "")

	srv := httptest.NewServer(NewHandler(memory.NewLocal(inmem.NewBackend()), reg, WithKG(kg)))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/_kg/nsA/sessA?action=search&q=hi", nil)
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Authorization", "Bearer tok-b")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a token scoped to a different session must not reach this scope's _kg route")
	assert.Equal(t, 0, kg.calls, "the KGQuerier must not be invoked for a cross-session request")
}

// TestHandleKGDoor_SameSessionToken_200 exercises the full mint path for a
// correctly-scoped per-session token: ServeHTTP mints a ReadKG approval for
// the URL scope, the new door passes it, and the provider is invoked with
// exactly the URL-derived scope.
func TestHandleKGDoor_SameSessionToken_200(t *testing.T) {
	kg := &fakeKGQuerier{}
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "nsA", Name: "sessA"}, "tok-a", "")

	srv := httptest.NewServer(NewHandler(memory.NewLocal(inmem.NewBackend()), reg, WithKG(kg)))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/_kg/nsA/sessA?action=search&q=hi", nil)
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Authorization", "Bearer tok-a")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "a correctly-scoped per-session token must be allowed")
	assert.Equal(t, 1, kg.calls, "the KGQuerier must be invoked exactly once")
	assert.Equal(t, memory.Scope{Kind: "session", ID: "nsA/sessA"}, kg.lastScope, "the provider must see the URL-derived scope")
}
