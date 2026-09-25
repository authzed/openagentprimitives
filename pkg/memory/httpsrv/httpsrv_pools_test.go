// pkg/memory/httpsrv/httpsrv_pools_test.go — the per-pool approval mint.
//
// ServeHTTP mints a session-scope approval at two sites (the webd branch and
// the per-session-bearer branch); this file pins that BOTH sites also mint
// approvals for the session's resource pools, discovered via mintPoolApprovals.
//
// The memory capability door keys on scope.ID alone (memory.EnsureApproval
// matches on (perm, resource), with no Kind), so authorizing a pool is
// entirely a minting question — CompositeSearcher.Search and Local.Query
// already check EnsureApproval(ReadMemory/WriteMemory, sc.ID) per scope with
// no changes needed here.
//
// Two kinds of assertion live here as a result. _search DOES dispatch pool
// scopes — handleSearch widens req.Scopes with the request's read pools — so
// those tests can assert on what the search actually spanned. Every OTHER
// route still forces its Query/Search Scope to the URL's own session and never
// names a pool, so for those the observable is the CONTEXT ServeHTTP built,
// pool approvals included; the ctxCapturingMemory wrapper below captures
// exactly that context so a test can reuse it for a follow-up call against the
// pool scope directly.
//
// package httpsrv (white-box): most tests here build *handler directly
// rather than going through WithPools, so the harness controls every field
// (mem, reg, pools) without needing a k8s client or artifact store — the
// other prerequisites NewHandler's other options would otherwise demand.
package httpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // registers the "turn" Kind this file seeds
	"github.com/authzed/openagentprimitives/pkg/memory/pools"
	"github.com/authzed/openagentprimitives/pkg/memory/search"
	searchinmem "github.com/authzed/openagentprimitives/pkg/memory/search/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

const (
	poolsTestNS   = "ns"
	poolsTestSess = "sess"
	poolsTestTok  = "tok-pools-1"
)

// poolFakeReader is a pools.RelationshipReader stub: a fixed relationship set
// or a fixed error, mirroring pkg/memory/pools' own fake reader. calls counts
// invocations, so a test can pin "resolved once per request" directly rather
// than inferring it from side effects.
type poolFakeReader struct {
	rels  []authz.Relation
	err   error
	calls int
}

func (f *poolFakeReader) ReadRelationships(_ context.Context, _ pools.RelFilter) ([]authz.Relation, error) {
	f.calls++
	return f.rels, f.err
}

// ctxCapturingMemory wraps a real memory.Memory and remembers the context the
// last Search/Query call arrived with. That context is what ServeHTTP built —
// pool approvals included — and it is how a test exercises those approvals
// against a resource scope on a route that does not name one itself: every
// route but _search forces its scope to the URL's own session.
type ctxCapturingMemory struct {
	memory.Memory
	lastCtx context.Context
}

func (m *ctxCapturingMemory) Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	m.lastCtx = ctx
	return m.Memory.Search(ctx, req)
}

func (m *ctxCapturingMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	m.lastCtx = ctx
	return m.Memory.Query(ctx, q)
}

// newPoolsHarness wires a real *memory.Local (inmem backend + a real
// CompositeSearcher over the inmem search provider — so the per-scope
// approval door under test is the actual production door, not a stub),
// wraps it in ctxCapturingMemory, and builds a *handler directly (bypassing
// NewHandler, which has no option for the pools field yet).
func newPoolsHarness(t *testing.T, reader pools.RelationshipReader) (*httptest.Server, *ctxCapturingMemory, *memory.Local, *tokens.Registry) {
	t.Helper()
	backend := meminmem.NewBackend()
	composite := search.New(search.WithProviders(searchinmem.New(backend)))
	real := memory.NewLocal(backend, memory.WithSearcher(composite))
	spy := &ctxCapturingMemory{Memory: real}
	reg := tokens.NewRegistry()
	h := &handler{mem: spy, reg: reg, pools: reader}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, spy, real, reg
}

func poolsAuthed(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func poolsDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

// seedPoolTurn puts a "turn" entry directly into scope, bypassing HTTP. turn
// is append-only, so this needs no approval at all — the entry only needs to
// EXIST so a search that reaches the backend would find it, distinguishing
// "the pool was excluded" from "the pool was empty."
func seedPoolTurn(t *testing.T, m *memory.Local, scope memory.Scope, id string) {
	t.Helper()
	_, err := m.Put(context.Background(), memory.Entry{
		Scope:     scope,
		Kind:      "turn",
		ID:        id,
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   []byte(`{"text":"hello"}`),
	})
	require.NoError(t, err, "seed pool entry")
}

// doSessionSearch issues an authed POST to the session's own _search route —
// the request whose ServeHTTP handling is under test — and returns the
// response and the context the search reached the wrapped Memory with.
//
// The request's own context is cancelled the instant Do returns (it is tied
// to the HTTP round trip's lifetime), so ctx.Err() is already
// context.Canceled by the time a test would reuse it for a follow-up
// in-process call. Neither CompositeSearcher nor the inmem provider consults
// cancellation today, so that reuse happens to work either way — but
// stripping it with context.WithoutCancel makes the reuse deliberate rather
// than accidental, and keeps these tests correct if a searcher later starts
// honouring ctx.Done().
func doSessionSearch(t *testing.T, srv *httptest.Server, spy *ctxCapturingMemory, token string) (*http.Response, context.Context) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/_search/"+poolsTestNS+"/"+poolsTestSess, strings.NewReader(`{}`))
	require.NoError(t, err, "NewRequest POST _search")
	req.Header.Set("Content-Type", "application/json")
	resp := poolsDo(t, poolsAuthed(req, token))
	require.NotNil(t, spy.lastCtx, "the search must have reached the wrapped Memory")
	return resp, context.WithoutCancel(spy.lastCtx)
}

// A session holding a slot on customer:alpha can search that pool. The door
// is EnsureApproval(ReadMemory, "customer:alpha"), so this passes only if the
// request minted an approval for the pool — not merely for the session.
func TestSearch_WithAPoolSlot_AuthorizesThePoolScope(t *testing.T) {
	poolScope, err := memory.ResourceScope("customer", "alpha")
	require.NoError(t, err)

	reader := &poolFakeReader{rels: []authz.Relation{
		authz.SlotGrantRelation("customer", "alpha", "read", authz.SessionRef{Namespace: poolsTestNS, Name: poolsTestSess}),
	}}
	srv, spy, real, reg := newPoolsHarness(t, reader)
	reg.Set(memory.NamespacedName{Namespace: poolsTestNS, Name: poolsTestSess}, poolsTestTok, "")
	seedPoolTurn(t, real, poolScope, "turn-1-user")

	resp, ctx := doSessionSearch(t, srv, spy, poolsTestTok)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the session's own search must still succeed")

	res, err := real.Search(ctx, memory.SearchRequest{
		Scopes: []memory.Scope{poolScope},
		Kinds:  []string{"turn"},
	})
	require.NoError(t, err, "a session holding a slot on the pool must be able to search it")
	var ids []string
	for _, e := range res.Entries {
		ids = append(ids, e.Entry.ID)
	}
	assert.Contains(t, ids, "turn-1-user", "the seeded pool entry must be found")
}

// The same session with NO slot is refused that pool. The pool is seeded
// with an entry that WOULD match, so the test distinguishes "pool excluded"
// from "pool empty" — the failure mode that makes this assertion vacuous.
func TestSearch_WithoutASlot_RefusesThePoolScope(t *testing.T) {
	poolScope, err := memory.ResourceScope("customer", "alpha")
	require.NoError(t, err)

	reader := &poolFakeReader{} // no relationships at all: no slot on anything
	srv, spy, real, reg := newPoolsHarness(t, reader)
	reg.Set(memory.NamespacedName{Namespace: poolsTestNS, Name: poolsTestSess}, poolsTestTok, "")
	seedPoolTurn(t, real, poolScope, "turn-1-user")

	resp, ctx := doSessionSearch(t, srv, spy, poolsTestTok)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the session's own search must still succeed with no pools held")

	_, err = real.Search(ctx, memory.SearchRequest{
		Scopes: []memory.Scope{poolScope},
		Kinds:  []string{"turn"},
	})
	require.Error(t, err, "a session with no slot on the pool must be refused, even though the pool holds a matching entry")
	assert.ErrorIs(t, err, memory.ErrMissingApproval)
}

// A write_memory slot mints a WriteMemory approval for the pool; a read-only
// slot does not. Asserted separately, because minting both from any slot is
// the natural implementation mistake and it silently grants egress.
func TestMint_ReadSlotDoesNotAuthorizeWritingThePool(t *testing.T) {
	poolScope, err := memory.ResourceScope("customer", "alpha")
	require.NoError(t, err)

	// "read" is an ordinary slot permission, not write_memory: it makes the
	// pool a READ pool only.
	reader := &poolFakeReader{rels: []authz.Relation{
		authz.SlotGrantRelation("customer", "alpha", "read", authz.SessionRef{Namespace: poolsTestNS, Name: poolsTestSess}),
	}}
	srv, spy, _, reg := newPoolsHarness(t, reader)
	reg.Set(memory.NamespacedName{Namespace: poolsTestNS, Name: poolsTestSess}, poolsTestTok, "")

	resp, ctx := doSessionSearch(t, srv, spy, poolsTestTok)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.NoError(t, memory.EnsureApproval(ctx, memory.ReadMemory, poolScope.ID),
		"a read-only slot still authorizes reading the pool")
	err = memory.EnsureApproval(ctx, memory.WriteMemory, poolScope.ID)
	require.Error(t, err, "a read-only slot must NOT authorize writing into the pool")
	assert.ErrorIs(t, err, memory.ErrMissingApproval)
}

// The stash and the mint must describe the SAME set, on every permission.
//
// withPoolReadScopes hands the request's read pools to a later handler on the
// same request, and poolReadScopesFrom's contract is that what comes back is
// what was approved — a scope-list expansion reading it names those scopes
// directly at the per-scope door. A WRITE request resolves the same Pools
// value (Read and Write are two views of one grant set) but mints only WRITE
// approvals, so stashing p.Read there leaves scopes stashed that this request
// holds no read approval for. Nothing reads it on a write route today, which
// is exactly why the invariant has to be in the code rather than in the fact
// that only one handler happens to call it.
func TestMint_AWriteRequestStashesNoReadPools(t *testing.T) {
	readOnly, err := memory.ResourceScope("customer", "alpha")
	require.NoError(t, err)
	writable, err := memory.ResourceScope("vendor", "beta")
	require.NoError(t, err)

	sess := authz.SessionRef{Namespace: poolsTestNS, Name: poolsTestSess}
	reader := &poolFakeReader{rels: []authz.Relation{
		// A read pool and a write pool, so p.Read is non-empty on both paths
		// and the assertion below cannot pass just because there was nothing
		// to stash.
		authz.SlotGrantRelation("customer", "alpha", "read", sess),
		authz.SlotGrantRelation("vendor", "beta", "write_memory", sess),
	}}
	h := &handler{pools: reader}

	write := h.mintPoolApprovals(context.Background(), poolsTestNS, poolsTestSess, poolsTestTok, memory.WriteMemory)
	assert.Nil(t, poolReadScopesFrom(write),
		"a write request approves no read pool, so it must stash none either")
	assert.NoError(t, memory.EnsureApproval(write, memory.WriteMemory, writable.ID),
		"the write mint itself is unaffected")

	read := h.mintPoolApprovals(context.Background(), poolsTestNS, poolsTestSess, poolsTestTok, memory.ReadMemory)
	stashed := poolReadScopesFrom(read)
	require.Len(t, stashed, 2, "a read request stashes every read pool — both grants make one")
	for _, sc := range stashed {
		assert.NoError(t, memory.EnsureApproval(read, memory.ReadMemory, sc.ID),
			"everything stashed must be approved on the same request: %s", sc.ID)
	}
	assert.Equal(t, []memory.Scope{readOnly, writable}, stashed, "sorted, so the expansion is reproducible")
}

// Pool discovery failing must degrade to session-only, not to an error and
// not to wider access.
func TestMint_PoolDiscoveryFailure_LeavesSessionScopeWorking(t *testing.T) {
	poolScope, err := memory.ResourceScope("customer", "alpha")
	require.NoError(t, err)

	// Had discovery succeeded, this relationship WOULD have made the pool a
	// read pool — so an implementation that widened access on failure (rather
	// than minting nothing) is caught by the same fixture that proves the
	// failure path mints nothing.
	reader := &poolFakeReader{
		rels: []authz.Relation{
			authz.SlotGrantRelation("customer", "alpha", "read", authz.SessionRef{Namespace: poolsTestNS, Name: poolsTestSess}),
		},
		err: errors.New("spicedb: read relationships: deadline exceeded"),
	}
	srv, spy, _, reg := newPoolsHarness(t, reader)
	reg.Set(memory.NamespacedName{Namespace: poolsTestNS, Name: poolsTestSess}, poolsTestTok, "")

	resp, ctx := doSessionSearch(t, srv, spy, poolsTestTok)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"a pool-discovery failure must not break an ordinary session-scope request")

	assert.NoError(t, memory.EnsureApproval(ctx, memory.ReadMemory, poolsTestNS+"/"+poolsTestSess),
		"the session's own approval must be unaffected by a pool-discovery failure")
	err = memory.EnsureApproval(ctx, memory.ReadMemory, poolScope.ID)
	require.Error(t, err, "a failed pool discovery must mint no pool approvals, not wider ones")
	assert.ErrorIs(t, err, memory.ErrMissingApproval)
}

// The webd branch is a SEPARATE mint site from the per-session-bearer branch
// (see ServeHTTP), and every test above only ever registers a per-session
// token, so it never enters the webd branch at all — deleting the webd
// mint call outright would leave every test above green. This test exists to
// close exactly that gap, and it also hosts the read/write asymmetry that
// matters most for a browser-facing, read-only credential: webd's OWN route
// is read-only, so even when the session holds a write_memory slot on the
// pool (making it a write pool), the mint must still be gated on the ROUTE's
// permission (ReadMemory here), not on what direction the pool happens to
// allow. Minting WriteMemory here — because "it's a write pool" rather than
// "this request is a read" — would hand a read-only, browser-facing
// credential write capability the moment some future route dispatches to a
// pool scope.
func TestSearch_WebdWithPoolSlot_AuthorizesReadNotWrite(t *testing.T) {
	poolScope, err := memory.ResourceScope("customer", "alpha")
	require.NoError(t, err)

	// write_memory: this pool IS a write pool for the session named in the
	// URL. webd's own route is still a read, so only ReadMemory may follow.
	reader := &poolFakeReader{rels: []authz.Relation{
		authz.SlotGrantRelation("customer", "alpha", "write_memory", authz.SessionRef{Namespace: poolsTestNS, Name: poolsTestSess}),
	}}
	srv, spy, real, reg := newPoolsHarness(t, reader)
	reg.SetWebdToken(poolsTestTok)
	seedPoolTurn(t, real, poolScope, "turn-1-user")

	resp, ctx := doSessionSearch(t, srv, spy, poolsTestTok)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "webd's own read-only search must still succeed")

	res, err := real.Search(ctx, memory.SearchRequest{
		Scopes: []memory.Scope{poolScope},
		Kinds:  []string{"turn"},
	})
	require.NoError(t, err, "webd must still be able to read a pool the session holds any slot on")
	var ids []string
	for _, e := range res.Entries {
		ids = append(ids, e.Entry.ID)
	}
	assert.Contains(t, ids, "turn-1-user")

	writeErr := memory.EnsureApproval(ctx, memory.WriteMemory, poolScope.ID)
	require.Error(t, writeErr, "webd must NEVER carry WriteMemory for a pool — its own route is read-only, even though this pool is a write pool for the session")
	assert.ErrorIs(t, writeErr, memory.ErrMissingApproval)
}

// A typed-nil concrete reader passed through WithPools must be normalized to
// a true nil interface, not left as a non-nil interface wrapping a nil
// value. poolFakeReader.ReadRelationships dereferences its receiver
// (f.rels), so a genuine nil-interface bypass would panic the moment
// mintPoolApprovals called it — this proves the guard trips BEFORE that call
// is ever reached, by asserting no panic at all.
func TestWithPools_TypedNilReaderIsNormalizedToNil(t *testing.T) {
	h := &handler{mem: memory.NewLocal(meminmem.NewBackend()), reg: tokens.NewRegistry()}

	var nilReader *poolFakeReader // typed nil; satisfies pools.RelationshipReader
	WithPools(nilReader)(h)

	require.NotPanics(t, func() {
		got := h.mintPoolApprovals(context.Background(), poolsTestNS, poolsTestSess, poolsTestTok, memory.ReadMemory)
		assert.Error(t, memory.EnsureApproval(got, memory.ReadMemory, "customer:alpha"),
			"a typed-nil reader must behave exactly like no reader configured: mint nothing")
	}, "a typed-nil concrete reader must be normalized to a true nil interface before ForSession is ever called")
}

// decodeSearchIDs decodes resp's body as a MergedSearchResult and returns the
// entry IDs it carries, for tests that assert on which scopes a plain _search
// over the HTTP route actually reached.
func decodeSearchIDs(t *testing.T, resp *http.Response) []string {
	t.Helper()
	var res memory.MergedSearchResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&res), "decode MergedSearchResult")
	ids := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		ids = append(ids, e.Entry.ID)
	}
	return ids
}

// A session with a slot on customer:alpha gets that pool's entries back from
// a plain _search over the HTTP route — without naming the pool anywhere in
// the request, because the server expands req.Scopes from the session's own
// read pools before dispatching to Search. Unlike the mint-only tests above,
// this asserts on the RESPONSE BODY itself: the earlier tests could only
// prove the approval was minted by reaching the pool scope with a manual
// follow-up call, because nothing yet made an HTTP _search dispatch there.
func TestSearch_ExpandsScopesWithTheSessionsReadPools(t *testing.T) {
	poolScope, err := memory.ResourceScope("customer", "alpha")
	require.NoError(t, err)

	reader := &poolFakeReader{rels: []authz.Relation{
		authz.SlotGrantRelation("customer", "alpha", "read", authz.SessionRef{Namespace: poolsTestNS, Name: poolsTestSess}),
	}}
	srv, spy, real, reg := newPoolsHarness(t, reader)
	reg.Set(memory.NamespacedName{Namespace: poolsTestNS, Name: poolsTestSess}, poolsTestTok, "")
	seedPoolTurn(t, real, poolScope, "turn-1-pool")

	resp, _ := doSessionSearch(t, srv, spy, poolsTestTok)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the session's own search must succeed")

	ids := decodeSearchIDs(t, resp)
	assert.Contains(t, ids, "turn-1-pool",
		"a plain _search over the session's own route must surface its read pool's entries without the body naming the pool")
}

// The body cannot introduce a scope. A request whose body names a pool the
// session holds NO slot on must not reach that pool — the overwrite is what
// guarantees it. The pool is seeded with an entry that WOULD match if the
// body's scopes were honored, so this fails whether or not the overwrite
// actually runs, rather than passing vacuously because the pool was empty.
func TestSearch_BodySuppliedScopesAreStillIgnored(t *testing.T) {
	namedScope, err := memory.ResourceScope("customer", "beta")
	require.NoError(t, err)

	// The session holds no slot on anything — customer:beta is reachable only
	// if the body's named scope is honored instead of discarded.
	reader := &poolFakeReader{}
	srv, _, real, reg := newPoolsHarness(t, reader)
	reg.Set(memory.NamespacedName{Namespace: poolsTestNS, Name: poolsTestSess}, poolsTestTok, "")
	seedPoolTurn(t, real, namedScope, "turn-1-named")

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/_search/"+poolsTestNS+"/"+poolsTestSess,
		strings.NewReader(`{"scopes":[{"kind":"resource","id":"customer:beta"}]}`))
	require.NoError(t, err, "NewRequest POST _search")
	req.Header.Set("Content-Type", "application/json")
	resp := poolsDo(t, poolsAuthed(req, poolsTestTok))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"a body-named scope must be silently discarded, not turned into a refusal or an error")

	ids := decodeSearchIDs(t, resp)
	assert.NotContains(t, ids, "turn-1-named",
		"a body-supplied scope must never be searched — only the session's own scope and its provable read pools may be")
}

// Pool-discovery failure must expand nothing and leave the session's own
// scope working — mirroring TestMint_PoolDiscoveryFailure_LeavesSessionScopeWorking,
// but asserting on the actual _search response rather than a manual
// follow-up EnsureApproval check, since the expansion (unlike the mint) has
// no other way to be observed from outside the package.
func TestSearch_PoolDiscoveryFailure_StillSearchesTheSessionScope(t *testing.T) {
	poolScope, err := memory.ResourceScope("customer", "alpha")
	require.NoError(t, err)

	// Had discovery succeeded, this relationship WOULD have made the pool a
	// read pool — so an implementation that widened access on failure (rather
	// than expanding nothing) is caught by the same fixture that proves the
	// session's own scope keeps working.
	reader := &poolFakeReader{
		rels: []authz.Relation{
			authz.SlotGrantRelation("customer", "alpha", "read", authz.SessionRef{Namespace: poolsTestNS, Name: poolsTestSess}),
		},
		err: errors.New("spicedb: read relationships: deadline exceeded"),
	}
	srv, spy, real, reg := newPoolsHarness(t, reader)
	reg.Set(memory.NamespacedName{Namespace: poolsTestNS, Name: poolsTestSess}, poolsTestTok, "")
	sessScope := memory.Scope{Kind: "session", ID: poolsTestNS + "/" + poolsTestSess}
	seedPoolTurn(t, real, sessScope, "turn-1-session")
	seedPoolTurn(t, real, poolScope, "turn-1-pool")

	resp, _ := doSessionSearch(t, srv, spy, poolsTestTok)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"a pool-discovery failure must not break an ordinary session-scope search")

	ids := decodeSearchIDs(t, resp)
	assert.Contains(t, ids, "turn-1-session", "the session's own scope must still be searched when pool discovery fails")
	assert.NotContains(t, ids, "turn-1-pool", "a failed pool discovery must expand nothing, not something")
}

// One SpiceDB read per request, not two: a request that both mints pool
// approvals (ServeHTTP, before dispatch) and expands the search scope list
// (handleSearch) must resolve pools.ForSession exactly once, by reusing the
// mint's own resolution rather than calling it again for the expansion.
func TestSearch_ResolvesPoolsOncePerRequest(t *testing.T) {
	reader := &poolFakeReader{rels: []authz.Relation{
		authz.SlotGrantRelation("customer", "alpha", "read", authz.SessionRef{Namespace: poolsTestNS, Name: poolsTestSess}),
	}}
	srv, spy, _, reg := newPoolsHarness(t, reader)
	reg.Set(memory.NamespacedName{Namespace: poolsTestNS, Name: poolsTestSess}, poolsTestTok, "")

	resp, _ := doSessionSearch(t, srv, spy, poolsTestTok)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, 1, reader.calls,
		"a request that both mints pool approvals and expands the search scope list must read SpiceDB once, not twice")
}
