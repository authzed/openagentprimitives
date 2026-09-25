package httpsrv_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register turn/lifecycle/... Kinds
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// lifecycleSetup wires the lifecycle Kind's hook to mem and registers
// teardown. lifecycle keeps a package-global facade reference, so a
// test that calls this must NOT run in parallel with another that does.
func lifecycleSetup(t *testing.T, mem memory.Memory) {
	t.Helper()
	lifecycle.Setup(mem)
	t.Cleanup(lifecycle.Teardown)
}

// newTestServer builds an httptest.Server over a fresh Local+inmem
// backend and returns the server, the underlying Local (for direct
// assertions), and the token registry.
func newTestServer(t *testing.T) (*httptest.Server, *memory.Local, *tokens.Registry) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg))
	t.Cleanup(srv.Close)
	return srv, mem, reg
}

func authed(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// do issues req and returns the response; the body is the caller's to
// close.
func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

// turnEntry builds a turn Entry body with the deterministic ID the turn
// Kind expects.
func turnEntry(id, text string) memory.Entry {
	content, _ := json.Marshal(map[string]string{"text": text})
	return memory.Entry{
		Kind:      "turn",
		ID:        id,
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   content,
	}
}

func TestPostEntry_Created_AndRetrievable(t *testing.T) {
	srv, _, reg := newTestServer(t)
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")

	body, err := json.Marshal(turnEntry("turn-0-user", "hello"))
	require.NoError(t, err, "Marshal")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/turn/ns/n", bytes.NewReader(body))
	require.NoError(t, err, "NewRequest POST")
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, authed(req, "tok-1"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST status=%d body=%s", resp.StatusCode, b)
	}
	var stored memory.Entry
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&stored), "decode stored Entry")
	assert.Equal(t, "turn-0-user", stored.ID, "stored ID")
	assert.Equal(t, "turn", stored.Kind, "stored Kind forced from URL")
	assert.Equal(t, memory.Scope{Kind: "session", ID: "ns/n"}, stored.Scope, "scope forced from URL")

	greq, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/turn/ns/n", nil)
	require.NoError(t, err, "NewRequest GET")
	gresp := do(t, authed(greq, "tok-1"))
	defer gresp.Body.Close()
	require.Equal(t, http.StatusOK, gresp.StatusCode, "GET status")
	var res memory.QueryResult
	require.NoError(t, json.NewDecoder(gresp.Body).Decode(&res), "decode QueryResult")
	require.Len(t, res.Entries, 1, "one entry listed")
	assert.Equal(t, "turn-0-user", res.Entries[0].ID, "listed entry ID")
}

// newTestServerWithDeleter builds an httptest.Server whose handler has
// the DELETE _entry route enabled (Local satisfies EntryDeleter).
func newTestServerWithDeleter(t *testing.T) (*httptest.Server, *memory.Local, *tokens.Registry) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg, httpsrv.WithDeleter(mem)))
	t.Cleanup(srv.Close)
	return srv, mem, reg
}

func TestPostEntry_AppendOnlyConflict_409(t *testing.T) {
	srv, _, reg := newTestServer(t)
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")

	post := func(text string) *http.Response {
		body, err := json.Marshal(turnEntry("turn-0-user", text))
		require.NoError(t, err, "Marshal")
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/turn/ns/n", bytes.NewReader(body))
		require.NoError(t, err, "NewRequest POST")
		req.Header.Set("Content-Type", "application/json")
		return do(t, authed(req, "tok-1"))
	}

	resp := post("hello")
	require.Equal(t, http.StatusCreated, resp.StatusCode, "first put → 201")
	resp.Body.Close()

	// Same ID, different content → append-only conflict → 409.
	resp = post("changed")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "conflicting re-put of append-only kind → 409")
}

func TestDeleteEntry_AppendOnlyKind_403(t *testing.T) {
	srv, mem, reg := newTestServerWithDeleter(t)
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")

	scope := memory.Scope{Kind: "session", ID: "ns/n"}
	_, err := mem.Put(context.Background(), memory.Entry{
		Scope: scope, Kind: "turn", ID: "turn-0-user",
		CreatedAt: time.Unix(0, 0).UTC(),
	})
	require.NoError(t, err, "seed append-only entry")

	req, err := http.NewRequest(http.MethodDelete,
		srv.URL+"/memory/_entry/ns/n?kind=turn&id=turn-0-user", nil)
	require.NoError(t, err, "NewRequest DELETE")
	resp := do(t, authed(req, "tok-1"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "DELETE on append-only kind → 403")
}

func TestPostEntry_EmptyID_ServerGenerates(t *testing.T) {
	srv, _, reg := newTestServer(t)
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")

	e := turnEntry("", "no-id") // empty ID → server generates via NewID
	body, err := json.Marshal(e)
	require.NoError(t, err, "Marshal")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/turn/ns/n", bytes.NewReader(body))
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, authed(req, "tok-1"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "POST with empty ID → 201")
	var stored memory.Entry
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&stored), "decode")
	assert.True(t, strings.HasPrefix(stored.ID, "turn-"), "server-generated ID has Kind prefix")
}

func TestQueryRoute_TagFilter(t *testing.T) {
	srv, mem, reg := newTestServer(t)
	scope := memory.Scope{Kind: "session", ID: "ns/n"}
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")

	// Seed two turn entries directly, one tagged.
	ctx := memory.WithSystemApproval(context.Background(), "test")
	_, err := mem.Put(ctx, memory.Entry{Scope: scope, Kind: "turn", ID: "turn-0-user", Tags: []string{"keep"}})
	require.NoError(t, err, "Put tagged")
	_, err = mem.Put(ctx, memory.Entry{Scope: scope, Kind: "turn", ID: "turn-1-user"})
	require.NoError(t, err, "Put untagged")

	q := memory.Query{Kinds: []string{"turn"}, Tags: []string{"keep"}}
	body, err := json.Marshal(q)
	require.NoError(t, err, "Marshal Query")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/_query/ns/n", bytes.NewReader(body))
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, authed(req, "tok-1"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "_query status")
	var res memory.QueryResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&res), "decode")
	require.Len(t, res.Entries, 1, "tag filter selects one entry")
	assert.Equal(t, "turn-0-user", res.Entries[0].ID, "tag-matched entry")
}

func TestSignalRoute_NoContent_AndHookRan(t *testing.T) {
	srv, mem, reg := newTestServer(t)
	scope := memory.Scope{Kind: "session", ID: "ns/n"}
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")

	// The lifecycle hook writes a lifecycle Entry for every signal. It
	// needs Setup wiring its facade — the operator does this in main;
	// here we wire it directly to the same Local the server holds.
	lifecycleSetup(t, mem)

	sig := memory.Signal{
		Kind: "lifecycle/session.started",
		At:   time.Unix(0, 0).UTC(),
	}
	body, err := json.Marshal(sig)
	require.NoError(t, err, "Marshal Signal")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/_signal/ns/n", bytes.NewReader(body))
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, authed(req, "tok-1"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "_signal → 204")

	// The lifecycle hook should have recorded the signal.
	res, err := mem.Query(memory.WithSystemApproval(context.Background(), "test"), memory.Query{
		Scope: scope, Kinds: []string{"lifecycle"},
	})
	require.NoError(t, err, "Query lifecycle")
	require.Len(t, res.Entries, 1, "lifecycle hook recorded the signal")
}

func TestPostEntry_UnknownKind_BadRequest(t *testing.T) {
	srv, _, reg := newTestServer(t)
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")

	body := strings.NewReader(`{"id":"x-1"}`)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/badkind/ns/n", body)
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, authed(req, "tok-1"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "unknown Kind → 400")
}

func TestAuth(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(reg *tokens.Registry)
		token      string // empty = no Authorization header
		wantStatus int
	}{
		{
			name:       "missing token: 401",
			setup:      func(*tokens.Registry) {},
			token:      "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "per-session token for wrong session: 403",
			setup: func(reg *tokens.Registry) {
				reg.Set(memory.NamespacedName{Namespace: "ns", Name: "other"}, "tok-other", "")
			},
			token:      "tok-other",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "per-session token for own session: 200",
			setup: func(reg *tokens.Registry) {
				reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-own", "")
			},
			token:      "tok-own",
			wantStatus: http.StatusOK,
		},
		{
			name: "channelsd system token: any session allowed",
			setup: func(reg *tokens.Registry) {
				reg.SetChannelsdToken("sys")
			},
			token:      "sys",
			wantStatus: http.StatusOK,
		},
		{
			name: "authzd system token: any session allowed",
			setup: func(reg *tokens.Registry) {
				reg.SetAuthzdToken("authzd-sys")
			},
			token:      "authzd-sys",
			wantStatus: http.StatusOK,
		},
		{
			name:       "unknown token: 401",
			setup:      func(*tokens.Registry) {},
			token:      "bogus",
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, reg := newTestServer(t)
			tc.setup(reg)
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/turn/ns/n", nil)
			require.NoError(t, err, "NewRequest")
			if tc.token != "" {
				authed(req, tc.token)
			}
			resp := do(t, req)
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode, "auth outcome")
		})
	}
}

// TestAuth_UnknownTokenSaysWhyItWasRefused pins the diagnosable half of the
// 401. The two ways to be refused with a bearer in hand — a wrong value, and a
// value that is still the one in the session's Secret while this process has
// never heard of it (an operator that restarted and did not re-register the
// session) — are indistinguishable to a caller reading "unauthorized", and the
// second cost hours of hunting a credential bug that was not there.
//
// Naming it adds no oracle: a REGISTERED token aimed at another session is
// answered 403, so the status code already separates known from unknown. The
// row below is that control. The body must never echo the token itself.
func TestAuth_UnknownTokenSaysWhyItWasRefused(t *testing.T) {
	const bogus = "not-a-registered-token"
	srv, _, reg := newTestServer(t)
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "other"}, "tok-other", "")

	get := func(t *testing.T, token string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/turn/ns/n", nil)
		require.NoError(t, err, "NewRequest")
		resp := do(t, authed(req, token))
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "read body")
		return resp.StatusCode, string(b)
	}

	status, body := get(t, bogus)
	require.Equal(t, http.StatusUnauthorized, status, "an unregistered bearer is a 401")
	assert.Contains(t, body, "not registered",
		"the 401 must say the bearer is unknown to this process, not merely 'unauthorized'")
	assert.NotContains(t, body, bogus, "the refusal must never echo the token value back")

	// Control: a token this process DOES know, aimed at a session it may not
	// reach, is a different answer already — which is why naming the 401 leaks
	// nothing new.
	status, _ = get(t, "tok-other")
	assert.Equal(t, http.StatusForbidden, status, "a registered bearer on the wrong session is a 403, not a 401")
}

func TestMethodNotAllowed(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "DELETE keyed route: 405", path: "/memory/turn/ns/n"},
		{name: "DELETE _query route: 405", path: "/memory/_query/ns/n"},
		{name: "DELETE _signal route: 405", path: "/memory/_signal/ns/n"},
		{name: "POST _scope route: 405 (in-process only)", path: "/memory/_scope/ns/n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, reg := newTestServer(t)
			reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "tok-1", "")
			method := http.MethodDelete
			if strings.Contains(tc.path, "_scope") {
				method = http.MethodPost
			}
			req, err := http.NewRequest(method, srv.URL+tc.path, nil)
			require.NoError(t, err, "NewRequest")
			resp := do(t, authed(req, "tok-1"))
			defer resp.Body.Close()
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "method rejected")
		})
	}
}

func TestMalformedPath_NotFound(t *testing.T) {
	srv, _, reg := newTestServer(t)
	reg.SetChannelsdToken("sys")
	_ = reg
	// Two segments instead of three.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/turn/ns", nil)
	require.NoError(t, err, "NewRequest")
	resp := do(t, authed(req, "sys"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "wrong segment count → 404")
}

// TestRun_ReturnsListenError verifies httpsrv.Run surfaces a bind
// failure (port already in use) verbatim instead of silently
// swallowing it.
func TestRun_ReturnsListenError(t *testing.T) {
	h := httpsrv.NewHandler(memory.NewLocal(inmem.NewBackend()), tokens.NewRegistry())

	first := httptest.NewServer(h)
	t.Cleanup(first.Close)
	addr := strings.TrimPrefix(first.URL, "http://")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := httpsrv.Run(ctx, addr, h)
	require.Error(t, err, "Run on a port already bound must return bind error")
	if !strings.Contains(err.Error(), "address already in use") &&
		!strings.Contains(err.Error(), "bind") {
		t.Logf("note: bind error message format may vary by platform: %v", err)
	}
}

// --- _publisher_key endpoint ---

// fakeRegistrar records the args of every RegisterPublisherKey call.
type fakeRegistrar struct {
	mu      sync.Mutex
	calls   int
	lastPub string
	lastKey string
	lastKID string
	returns error
}

func (r *fakeRegistrar) RegisterPublisherKey(_ context.Context, publisher, keyID string, pub ed25519.PublicKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastPub, r.lastKID, r.lastKey = publisher, keyID, string(pub)
	return r.returns
}

// pubKeyB64 returns a deterministic Ed25519 public key (base64-std) and
// its provenance KeyID.
func pubKeyB64(seed byte) (b64, keyID string, pub ed25519.PublicKey) {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	pub = ed25519.NewKeyFromSeed(s).Public().(ed25519.PublicKey)
	return base64.StdEncoding.EncodeToString(pub), provenance.KeyID(pub), pub
}

func TestPublisherKeyEndpoint(t *testing.T) {
	b64, keyID, pub := pubKeyB64(7)
	wrongID := "00000000000000000000000000000000"

	cases := []struct {
		name       string
		withReg    bool
		setup      func(reg *tokens.Registry)
		token      string
		body       string
		wantStatus int
		wantPub    string // expected publisher captured by registrar ("" = no call expected)
	}{
		{
			name:       "channelsd token: 204, registrar called with system:channelsd",
			withReg:    true,
			setup:      func(reg *tokens.Registry) { reg.SetChannelsdToken("chan-tok") },
			token:      "chan-tok",
			body:       `{"keyId":"` + keyID + `","pubKey":"` + b64 + `"}`,
			wantStatus: http.StatusNoContent,
			wantPub:    "system:channelsd",
		},
		{
			name:       "authzd token: 204, registrar called with system:authzd",
			withReg:    true,
			setup:      func(reg *tokens.Registry) { reg.SetAuthzdToken("authzd-tok") },
			token:      "authzd-tok",
			body:       `{"keyId":"` + keyID + `","pubKey":"` + b64 + `"}`,
			wantStatus: http.StatusNoContent,
			wantPub:    "system:authzd",
		},
		{
			name:    "per-session token: 403, not registered",
			withReg: true,
			setup: func(reg *tokens.Registry) {
				reg.Set(memory.NamespacedName{Namespace: "ns", Name: "n"}, "sess-tok", "")
			},
			token:      "sess-tok",
			body:       `{"keyId":"` + keyID + `","pubKey":"` + b64 + `"}`,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "webd read-only token: 403, cannot register a publisher key",
			withReg:    true,
			setup:      func(reg *tokens.Registry) { reg.SetWebdToken("webd-tok") },
			token:      "webd-tok",
			body:       `{"keyId":"` + keyID + `","pubKey":"` + b64 + `"}`,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "bad base64 pubKey: 400",
			withReg:    true,
			setup:      func(reg *tokens.Registry) { reg.SetChannelsdToken("chan-tok") },
			token:      "chan-tok",
			body:       `{"keyId":"` + keyID + `","pubKey":"not base64!!"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "wrong-length key: 400",
			withReg:    true,
			setup:      func(reg *tokens.Registry) { reg.SetChannelsdToken("chan-tok") },
			token:      "chan-tok",
			body:       `{"keyId":"` + keyID + `","pubKey":"` + base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) + `"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "keyId mismatch: 400",
			withReg:    true,
			setup:      func(reg *tokens.Registry) { reg.SetChannelsdToken("chan-tok") },
			token:      "chan-tok",
			body:       `{"keyId":"` + wrongID + `","pubKey":"` + b64 + `"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "no registrar configured: 405",
			withReg:    false,
			setup:      func(reg *tokens.Registry) { reg.SetChannelsdToken("chan-tok") },
			token:      "chan-tok",
			body:       `{"keyId":"` + keyID + `","pubKey":"` + b64 + `"}`,
			wantStatus: http.StatusMethodNotAllowed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			reg := tokens.NewRegistry()
			tc.setup(reg)
			fr := &fakeRegistrar{}
			var opts []httpsrv.HandlerOption
			if tc.withReg {
				opts = append(opts, httpsrv.WithPublisherKeyRegistrar(fr))
			}
			srv := httptest.NewServer(httpsrv.NewHandler(mem, reg, opts...))
			t.Cleanup(srv.Close)

			req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/_publisher_key", strings.NewReader(tc.body))
			require.NoError(t, err, "NewRequest")
			req.Header.Set("Content-Type", "application/json")
			resp := do(t, authed(req, tc.token))
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode, "status")

			if tc.wantPub != "" {
				require.Equal(t, 1, fr.calls, "registrar called once")
				assert.Equal(t, tc.wantPub, fr.lastPub, "publisher derived from token")
				assert.Equal(t, keyID, fr.lastKID, "keyID forwarded")
				assert.Equal(t, string(pub), fr.lastKey, "public key forwarded")
			} else {
				assert.Zero(t, fr.calls, "registrar must NOT be called on a rejected request")
			}
		})
	}
}

// TestWebdToken_ReadOnly proves the webd system token reads any session but
// is refused on every mutating route — webd is browser-facing and must not be
// able to write or fork the audit log.
func TestWebdToken_ReadOnly(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetWebdToken("webd")
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg, httpsrv.WithDeleter(mem)))
	t.Cleanup(srv.Close)

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"GET kind list: allowed", http.MethodGet, "/memory/turn/ns/n", "", http.StatusOK},
		{"POST _query (read): allowed", http.MethodPost, "/memory/_query/ns/n", `{}`, http.StatusOK},
		{"POST entry append: denied", http.MethodPost, "/memory/turn/ns/n", `{"kind":"turn","id":"turn-1-user","content":{"text":"x"}}`, http.StatusForbidden},
		{"POST _signal: denied", http.MethodPost, "/memory/_signal/ns/n", `{"kind":"x"}`, http.StatusForbidden},
		{"POST _reindex: denied", http.MethodPost, "/memory/_reindex/ns/n", "", http.StatusForbidden},
		{"DELETE _entry: denied", http.MethodDelete, "/memory/_entry/ns/n", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, body)
			require.NoError(t, err, "NewRequest")
			resp := do(t, authed(req, "webd"))
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode, "%s %s", tc.method, tc.path)
		})
	}
}

// capturedCtxMemory records the context the auth layer built for the
// backend call it observes, so a test can interrogate the capability that
// was minted rather than trying to stage a cross-session read the handlers
// already prevent by forcing the scope from the URL. It also satisfies
// httpsrv.KGQuerier so the _kg route — which reads h.kg, not h.mem — can be
// probed through the same fixture; only SearchFacts is exercised by the
// routes under test, the rest are required by the interface.
type capturedCtxMemory struct {
	memory.Memory
	ctx context.Context
}

func (c *capturedCtxMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	c.ctx = ctx
	return memory.QueryResult{}, nil
}

func (c *capturedCtxMemory) Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	c.ctx = ctx
	return memory.MergedSearchResult{}, nil
}

func (c *capturedCtxMemory) SearchFacts(ctx context.Context, query string, limit int) ([]memory.KGFact, error) {
	c.ctx = ctx
	return nil, nil
}

func (c *capturedCtxMemory) GetEntity(ctx context.Context, uuid string) (*memory.KGEntity, error) {
	return nil, nil
}

func (c *capturedCtxMemory) EntityFacts(ctx context.Context, entityUUID string) ([]memory.KGFact, error) {
	return nil, nil
}

func (c *capturedCtxMemory) RelatedEntities(ctx context.Context, entityUUID string, limit int) ([]memory.KGEntity, error) {
	return nil, nil
}

func (c *capturedCtxMemory) Communities(ctx context.Context, groupID string) ([]memory.KGCommunity, error) {
	return nil, nil
}

// newHandlerWithWebdToken builds a handler over mem with a single
// registered webd token, for tests that need to inspect the context the
// auth layer attaches to a backend call rather than staging state through
// the real Local facade. When mem also satisfies httpsrv.KGQuerier (as
// capturedCtxMemory does), it's wired as the KG provider too so the _kg
// route is reachable.
func newHandlerWithWebdToken(t *testing.T, mem memory.Memory) (http.Handler, string) {
	t.Helper()
	reg := tokens.NewRegistry()
	reg.SetWebdToken("webd-tok")
	var opts []httpsrv.HandlerOption
	if kg, ok := mem.(httpsrv.KGQuerier); ok {
		opts = append(opts, httpsrv.WithKG(kg))
	}
	return httpsrv.NewHandler(mem, reg, opts...), "webd-tok"
}

func TestWebdTokenMintsASessionScopedApproval(t *testing.T) {
	cap := &capturedCtxMemory{}
	h, webdToken := newHandlerWithWebdToken(t, cap)

	req := httptest.NewRequest(http.MethodPost, "/memory/_query/demo-ns/demo-session", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+webdToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, cap.ctx, "the handler must have reached the backend")

	// PERMITS the URL-path session.
	assert.NoError(t, memory.EnsureApproval(cap.ctx, memory.ReadMemory, "demo-ns/demo-session"),
		"the session named in the URL path must be readable")

	// REFUSES anything else. These are the RED assertions: today's wildcard
	// WithSystemApproval satisfies every one of them.
	assert.ErrorIs(t, memory.EnsureApproval(cap.ctx, memory.ReadMemory, "other-ns/other-session"),
		memory.ErrMissingApproval, "a different session must not be readable")
	assert.ErrorIs(t, memory.EnsureApproval(cap.ctx, memory.ReadMemory, "demo-ns/other-session"),
		memory.ErrMissingApproval, "a sibling session in the same namespace must not be readable")
	assert.ErrorIs(t, memory.EnsureApproval(cap.ctx, memory.ReadMemory, ""),
		memory.ErrMissingApproval, "the empty resource (all scopes) must not be readable")
	assert.ErrorIs(t, memory.EnsureApproval(cap.ctx, memory.WriteMemory, "demo-ns/demo-session"),
		memory.ErrMissingApproval, "the read route must not carry a write capability")
	assert.ErrorIs(t, memory.EnsureApproval(cap.ctx, memory.DeleteMemory, "demo-ns/demo-session"),
		memory.ErrMissingApproval, "the read route must not carry a delete capability")
}

func TestWebdTokenPermissionMatchesTheRoute(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   memory.Permission
	}{
		{name: "_query mints ReadMemory", method: http.MethodPost, path: "/memory/_query/demo-ns/demo-session", want: memory.ReadMemory},
		{name: "_search mints ReadMemory", method: http.MethodPost, path: "/memory/_search/demo-ns/demo-session", want: memory.ReadMemory},
		{name: "_kg mints ReadKG", method: http.MethodPost, path: "/memory/_kg/demo-ns/demo-session?action=search", want: memory.ReadKG},
		{name: "GET {kind} mints ReadMemory", method: http.MethodGet, path: "/memory/note/demo-ns/demo-session", want: memory.ReadMemory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &capturedCtxMemory{}
			h, webdToken := newHandlerWithWebdToken(t, cap)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+webdToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code,
				"every webd-reachable route must still succeed for its own session")
			require.NotNil(t, cap.ctx)
			assert.NoError(t, memory.EnsureApproval(cap.ctx, tc.want, "demo-ns/demo-session"))
		})
	}
}

func TestWebdTokenReadOnlyRefusalStillHolds(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{name: "_signal is refused", method: http.MethodPost, path: "/memory/_signal/demo-ns/demo-session"},
		{name: "_entry delete is refused", method: http.MethodDelete, path: "/memory/_entry/demo-ns/demo-session"},
		{name: "_reindex is refused", method: http.MethodPost, path: "/memory/_reindex/demo-ns/demo-session"},
		{name: "POST {kind} is refused", method: http.MethodPost, path: "/memory/note/demo-ns/demo-session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &capturedCtxMemory{}
			h, webdToken := newHandlerWithWebdToken(t, cap)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+webdToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusForbidden, rec.Code,
				"the read-only route gate must be unchanged by the scoping change")
		})
	}
}

func TestPublisherKeyEndpoint_MethodNotAllowed(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("chan-tok")
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg, httpsrv.WithPublisherKeyRegistrar(&fakeRegistrar{})))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/memory/_publisher_key", nil)
	require.NoError(t, err, "NewRequest")
	resp := do(t, authed(req, "chan-tok"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "GET not allowed on _publisher_key")
}

// TestRun_ShutsDownOnContextCancel verifies cancelling the ctx passed
// to Run causes a clean shutdown without an error return.
func TestRun_ShutsDownOnContextCancel(t *testing.T) {
	h := httpsrv.NewHandler(memory.NewLocal(inmem.NewBackend()), tokens.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())

	temp := httptest.NewServer(h)
	addr := strings.TrimPrefix(temp.URL, "http://")
	temp.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- httpsrv.Run(ctx, addr, h) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err, "Run after ctx cancel should return nil")
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after ctx cancel")
	}
}
