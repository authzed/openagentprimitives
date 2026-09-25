package debug_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	memory_pkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/x/debug"
)

const testToken = "s3kr1t-test-token"

// stubExtra is a minimal http.Handler used to assert that
// NewHandlerWithMemAuth's `extra` is reachable through the debug mux at the
// memHandler prefixes channelsd and webd depend on — /memory/ (session
// data), /artifact/ (channelsd's asset fetcher), /artifact-bundle/ (webd's
// ZIP download), and /inbound-asset/ (channelsd's attachment upload) — plus
// one wholly invented prefix, proving the mux forwards by catch-all rather
// than by a maintained per-prefix list: a case here can never go stale the
// way an enumerated Handle() call can.
type stubExtra struct {
	gotPath string
}

func (s *stubExtra) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.gotPath = r.URL.Path
	w.WriteHeader(http.StatusTeapot) // distinctive marker
}

func TestNewHandlerWithMemAuth_MountsExtraAtMemoryAndArtifact(t *testing.T) {
	store := blobstore.NewMem()
	stub := &stubExtra{}
	reg := tokens.NewRegistry()
	mux := debug.NewHandlerWithMemAuth(store, testToken, stub, reg)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cases := []struct {
		name string
		path string
	}{
		{"memory route reaches extra", "/memory/sessions/foo"},
		{"artifact route reaches extra", "/artifact/ns/sess/render/output"},
		{"artifact-bundle (ZIP download) route reaches extra", "/artifact-bundle/ns/sess/render/bundle"},
		{"inbound-asset (attachment upload) route reaches extra", "/inbound-asset/ns/sess"},
		{"an unenumerated future prefix still reaches extra via the catch-all", "/some-future-route/ns/sess"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub.gotPath = ""
			resp, err := http.Get(srv.URL + tc.path)
			require.NoError(t, err, "GET %s", tc.path)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusTeapot, resp.StatusCode, "handler reached (418 marker)")
			assert.Equal(t, tc.path, stub.gotPath, "handler observed path")
		})
	}
}

// TestNewHandlerWithMemAuth_ForwardsEveryHTTPSrvRoute wires the REAL
// httpsrv.NewHandler — not stubExtra — the way internal/cmd/operator does
// (httpsrv.WithArtifact enables /artifact/, /artifact-bundle/, AND
// /inbound-asset/; see httpsrv.NewHandler's option gating). A stub can only
// prove the debug mux forwards to whatever paths the test author remembered
// to list; it cannot catch a divergence between that list and httpsrv's
// actual route set, which is exactly how /inbound-asset/ went unreachable in
// production: /memory/, /artifact/, and /artifact-bundle/ were each verified
// against a stub, /inbound-asset/ never was, and nothing here compared the
// two handlers directly.
//
// Every route memHandler serves checks its bearer token before doing
// anything else (handler.ServeHTTP's prefix check for /memory/;
// checkSystemBearer for /artifact/, /artifact-bundle/, and /inbound-asset/ —
// see pkg/memory/httpsrv), so an unauthenticated request that reaches the
// route always answers 401. Only Go's ServeMux failing to route the request
// at all answers 404. A route the debug wrapper forwards therefore answers
// identically (401) whether hit directly against memHandler or through the
// wrapper; a route the wrapper silently drops answers 404 only through the
// wrapper — the exact split this test asserts against.
func TestNewHandlerWithMemAuth_ForwardsEveryHTTPSrvRoute(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := blobstore.NewMem()
	mem := memory_pkg.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	memHandler := httpsrv.NewHandler(mem, reg, httpsrv.WithArtifact(k8sClient, store))

	direct := httptest.NewServer(memHandler)
	t.Cleanup(direct.Close)
	wrapped := httptest.NewServer(debug.NewHandlerWithMemAuth(store, testToken, memHandler, reg))
	t.Cleanup(wrapped.Close)

	paths := []string{
		"/memory/turn/ns/sess",
		"/artifact/ns/sess/render/output",
		"/artifact-bundle/ns/sess/render/bundle",
		"/inbound-asset/ns/sess",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			directResp, err := http.Get(direct.URL + p)
			require.NoError(t, err, "GET %s directly against memHandler", p)
			defer directResp.Body.Close()
			require.Equal(t, http.StatusUnauthorized, directResp.StatusCode,
				"sanity: memHandler itself must serve %s (401, not 404) for this comparison to mean anything", p)

			wrappedResp, err := http.Get(wrapped.URL + p)
			require.NoError(t, err, "GET %s through the debug wrapper", p)
			defer wrappedResp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, wrappedResp.StatusCode,
				"%s: debug wrapper must forward to memHandler rather than 404 it locally", p)
		})
	}
}

func newTestServer(t *testing.T) (*httptest.Server, *blobstore.Store) {
	t.Helper()
	store := blobstore.NewMem()
	srv := httptest.NewServer(debug.NewHandler(store, testToken))
	t.Cleanup(srv.Close)
	return srv, store
}

func authed(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

func TestArtifactHandler_OK(t *testing.T) {
	srv, store := newTestServer(t)
	ref, err := store.Put(context.Background(), "k1", strings.NewReader("hello"))
	require.NoError(t, err, "Put")
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/debug/artifact?ref="+url.QueryEscape(string(ref)), nil)
	require.NoError(t, err, "NewRequest")
	resp, err := http.DefaultClient.Do(authed(req))
	require.NoError(t, err, "GET")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "status")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "ReadAll")
	assert.Equal(t, "hello", string(body), "body")
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
}

// TestArtifactHandler_ErrorCases covers the four non-2xx paths
// (missing ref → 400, unknown ref → 404, no auth → 401, wrong token →
// 401). Each row varies one of three knobs: query string, header
// value, presence of header.
func TestArtifactHandler_ErrorCases(t *testing.T) {
	cases := []struct {
		name                string
		query               string // appended after /debug/artifact
		authHeader          string // empty means no Authorization header
		wantStatus          int
		wantWWWAuthenticate bool
	}{
		{
			name:       "missing ref returns 400",
			query:      "",
			authHeader: "Bearer " + testToken,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown ref returns 404",
			query:      "?ref=mem://missing",
			authHeader: "Bearer " + testToken,
			wantStatus: http.StatusNotFound,
		},
		{
			name:                "no auth returns 401 with WWW-Authenticate",
			query:               "?ref=mem://x",
			authHeader:          "",
			wantStatus:          http.StatusUnauthorized,
			wantWWWAuthenticate: true,
		},
		{
			name:       "wrong token returns 401",
			query:      "?ref=mem://x",
			authHeader: "Bearer wrong-token",
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/debug/artifact"+tc.query, nil)
			require.NoError(t, err, "NewRequest")
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err, "GET")
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode, "status")
			if tc.wantWWWAuthenticate {
				assert.NotEmpty(t, resp.Header.Get("WWW-Authenticate"), "WWW-Authenticate header")
			}
		})
	}
}

func TestNewHandler_EmptyTokenPanics(t *testing.T) {
	assert.Panics(t, func() {
		_ = debug.NewHandler(blobstore.NewMem(), "")
	}, "empty token must panic")
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err, "GET")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "/healthz is unauthenticated")
}

// artifactListResponse mirrors the JSON payload from /debug/artifacts.
type artifactListResponse struct {
	Items []struct {
		Ref       string `json:"ref"`
		Key       string `json:"key"`
		Size      int64  `json:"size"`
		CreatedAt string `json:"createdAt"`
	} `json:"items"`
	Continue string `json:"continue"`
}

func TestArtifactListHandler_OK(t *testing.T) {
	srv, store := newTestServer(t)
	ctx := context.Background()
	_, err := store.Put(ctx, "key-a", strings.NewReader("hello"))
	require.NoError(t, err, "Put key-a")
	_, err = store.Put(ctx, "key-b", strings.NewReader("world!"))
	require.NoError(t, err, "Put key-b")

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/debug/artifacts", nil)
	require.NoError(t, err, "NewRequest")
	resp, err := http.DefaultClient.Do(authed(req))
	require.NoError(t, err, "GET")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "status")
	assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"),
		"Content-Type=%q", resp.Header.Get("Content-Type"))

	var payload artifactListResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload), "decode JSON")
	require.Len(t, payload.Items, 2, "2 items")
	assert.Empty(t, payload.Continue, "continue token")
	// Items must be sorted alphabetically by key.
	assert.Equal(t, "key-a", payload.Items[0].Key, "items[0].Key")
	assert.Equal(t, "key-b", payload.Items[1].Key, "items[1].Key")
	assert.Equal(t, int64(len("hello")), payload.Items[0].Size, "items[0].Size")
	assert.NotEmpty(t, payload.Items[0].Ref, "items[0].Ref")
	assert.NotEmpty(t, payload.Items[0].CreatedAt, "items[0].CreatedAt")
}

func TestArtifactListHandler_PrefixFilter(t *testing.T) {
	srv, store := newTestServer(t)
	ctx := context.Background()
	for _, k := range []string{"ns-a/obj-1", "ns-a/obj-2", "ns-b/obj-1"} {
		_, err := store.Put(ctx, k, strings.NewReader("data"))
		require.NoErrorf(t, err, "Put %s", k)
	}

	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/debug/artifacts?prefix="+url.QueryEscape("ns-a/"), nil)
	require.NoError(t, err, "NewRequest")
	resp, err := http.DefaultClient.Do(authed(req))
	require.NoError(t, err, "GET")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "status")

	var payload artifactListResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload), "decode JSON")
	require.Len(t, payload.Items, 2, "prefix ns-a/ matches 2 items")
	for _, item := range payload.Items {
		assert.Truef(t, strings.HasPrefix(item.Key, "ns-a/"),
			"item key %q does not match prefix", item.Key)
	}
}

func TestArtifactListHandler_NoAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/debug/artifacts")
	require.NoError(t, err, "GET")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "no auth → 401")
}

func TestArtifactPerSessionTokenMatchesRef(t *testing.T) {
	store := blobstore.NewMem()
	reg := tokens.NewRegistry()
	sess := memory_pkg.NamespacedName{Namespace: "default", Name: "s1"}
	reg.Set(sess, "sess-token-1", "")

	srv := httptest.NewServer(debug.NewHandlerWithMemAuth(store, "global-debug-token", nil, reg))
	t.Cleanup(srv.Close)

	// Put an artifact whose key matches the session's namespace + name.
	ref, err := store.Put(context.Background(), "default/s1/uid/stdout", strings.NewReader("hello"))
	require.NoError(t, err, "Put")

	// Per-session token can read it.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/debug/artifact?ref="+url.QueryEscape(string(ref)), nil)
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Authorization", "Bearer sess-token-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "per-session token must succeed")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "ReadAll")
	assert.Equal(t, "hello", string(body), "body")
	// serveArtifact (the dual-auth path) must set the same headers
	// artifactHandler does — the two writers had drifted apart.
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
}

func TestArtifactPerSessionTokenRejectedForOtherSessionRef(t *testing.T) {
	store := blobstore.NewMem()
	reg := tokens.NewRegistry()
	reg.Set(memory_pkg.NamespacedName{Namespace: "default", Name: "s1"}, "sess-token-1", "")

	srv := httptest.NewServer(debug.NewHandlerWithMemAuth(store, "global", nil, reg))
	t.Cleanup(srv.Close)

	// Put an artifact under a DIFFERENT session.
	ref, err := store.Put(context.Background(), "default/s2/uid/stdout", strings.NewReader("not yours"))
	require.NoError(t, err, "Put")

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/debug/artifact?ref="+url.QueryEscape(string(ref)), nil)
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Authorization", "Bearer sess-token-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"ref's session is s2 but token is s1: must be 401")
}
