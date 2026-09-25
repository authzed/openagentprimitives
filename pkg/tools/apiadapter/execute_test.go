package apiadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useTestClient points the engine at an unguarded client, and disarms the
// resolved-host re-guard, so an httptest server (loopback — which both
// safehttp.Client() and safehttp.GuardHost correctly refuse) is reachable.
// Both are package-level state, so callers of this helper must NOT
// t.Parallel().
func useTestClient(t *testing.T) {
	t.Helper()
	origClient, origGuard := newHTTPClient, guardHost
	newHTTPClient = func() *http.Client { return &http.Client{Timeout: 5 * time.Second} }
	guardHost = func(string) error { return nil }
	t.Cleanup(func() {
		newHTTPClient = origClient
		guardHost = origGuard
	})
}

// cfgFor builds a validated config pointed at srv, bypassing Validate's
// baseURL host guard (which refuses loopback) by constructing directly.
func cfgFor(srv *httptest.Server, auth Auth, ops ...Operation) Config {
	return Config{BaseURL: srv.URL, Auth: auth, Operations: ops}
}

func TestCall_BindsPathQueryHeaderAndBody(t *testing.T) {
	useTestClient(t)
	var gotPath, gotQuery, gotHeader string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath, not Path: net/http decodes %-escapes into r.URL.Path,
		// so asserting the param landed percent-encoded on the wire needs the
		// still-escaped form.
		gotPath, gotQuery, gotHeader = r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("X-Trace")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	op := Operation{Name: "put_note", Method: "POST", Path: "/accounts/{id}/notes", Params: []Param{
		{Name: "id", In: "path", Required: true, Type: "string"},
		{Name: "verbose", In: "query", Type: "boolean"},
		{Name: "trace", In: "header", Type: "string"},
		{Name: "text", In: "body", Required: true, Type: "string"},
	}}
	// The header param's wire name is its Name; use X-Trace to prove it.
	op.Params[2].Name = "X-Trace"

	e := NewExecutor(cfgFor(srv, Auth{Type: "none"}, op), "")
	res, err := e.Call(context.Background(), "put_note", map[string]any{
		"id": "acct 42", "verbose": true, "X-Trace": "t-1", "text": "hello",
	})
	require.NoError(t, err)
	assert.Equal(t, 200, res.Status)
	assert.Equal(t, "/accounts/acct%2042/notes", gotPath, "path param is escaped")
	assert.Equal(t, "verbose=true", gotQuery)
	assert.Equal(t, "t-1", gotHeader)
	assert.Equal(t, map[string]any{"text": "hello"}, gotBody, "only body params land in the body")
}

func TestCall_AuthSchemes(t *testing.T) {
	cases := []struct {
		name  string
		auth  Auth
		check func(t *testing.T, r *http.Request)
	}{
		{"bearer", Auth{Type: "bearer", EnvVar: "T"}, func(t *testing.T, r *http.Request) {
			assert.Equal(t, "Bearer sekret", r.Header.Get("Authorization"))
		}},
		{"header", Auth{Type: "header", EnvVar: "T", Name: "X-Api-Key"}, func(t *testing.T, r *http.Request) {
			assert.Equal(t, "sekret", r.Header.Get("X-Api-Key"))
		}},
		{"basic", Auth{Type: "basic", EnvVar: "T"}, func(t *testing.T, r *http.Request) {
			u, p, ok := r.BasicAuth()
			require.True(t, ok)
			assert.Equal(t, "user", u)
			assert.Equal(t, "pw", p)
		}},
		{"query", Auth{Type: "query", EnvVar: "T", Name: "api_key"}, func(t *testing.T, r *http.Request) {
			assert.Equal(t, "sekret", r.URL.Query().Get("api_key"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTestClient(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tc.check(t, r) }))
			t.Cleanup(srv.Close)
			cred := "sekret"
			if tc.auth.Type == "basic" {
				cred = "user:pw"
			}
			e := NewExecutor(cfgFor(srv, tc.auth, Operation{Name: "go", Method: "GET", Path: "/x"}), cred)
			_, err := e.Call(context.Background(), "go", nil)
			require.NoError(t, err)
		})
	}
}

// TestCall_AuthOverwritesModelSuppliedAuthorizationHeader pins the
// credential-overwrite direction: injectAuth runs AFTER the param loop, so a
// model-supplied "Authorization" argument is overwritten by the configured
// credential, never the reverse. Reordering those two steps would silently
// let a model argument win over the injected credential.
func TestCall_AuthOverwritesModelSuppliedAuthorizationHeader(t *testing.T) {
	useTestClient(t)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	op := Operation{Name: "go", Method: "GET", Path: "/x", Params: []Param{
		{Name: "Authorization", In: "header", Type: "string"},
	}}
	e := NewExecutor(cfgFor(srv, Auth{Type: "bearer", EnvVar: "T"}, op), "sekret")
	_, err := e.Call(context.Background(), "go", map[string]any{"Authorization": "attacker"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer sekret", gotAuth, "the injected credential must win over a model-supplied Authorization header")
}

func TestCall_NonTwoXXIsNotAnError(t *testing.T) {
	useTestClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"no such account"}`))
	}))
	t.Cleanup(srv.Close)

	e := NewExecutor(cfgFor(srv, Auth{Type: "none"}, Operation{Name: "go", Method: "GET", Path: "/x"}), "")
	res, err := e.Call(context.Background(), "go", nil)
	require.NoError(t, err, "an upstream 404 is a result, not a transport error")
	assert.Equal(t, 404, res.Status)
	assert.Contains(t, string(res.Body), "no such account")
}

func TestCall_Errors(t *testing.T) {
	useTestClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	op := Operation{Name: "go", Method: "GET", Path: "/x/{id}", Params: []Param{{Name: "id", In: "path", Required: true, Type: "string"}}}

	e := NewExecutor(cfgFor(srv, Auth{Type: "bearer", EnvVar: "T"}, op), "")
	_, err := e.Call(context.Background(), "nope", nil)
	require.ErrorContains(t, err, "unknown operation")

	_, err = e.Call(context.Background(), "go", map[string]any{})
	require.ErrorContains(t, err, "missing required argument")

	_, err = e.Call(context.Background(), "go", map[string]any{"id": "1"})
	require.ErrorContains(t, err, "credential is empty", "auth configured but no credential supplied")
}

func TestCall_RefusesGuardedDestination(t *testing.T) {
	// Pins the DIAL-TIME guard inside the real safehttp.Client() — the
	// authoritative backstop — so the guardHost pre-check is disarmed here.
	// Without that swap this literal-IP host is refused by the pre-check in
	// buildRequest and the dialer's own refusal is never exercised; the
	// pre-check has its own isolated test
	// (TestBuildRequest_ReguardsResolvedHostForUnvalidatedConfig), which swaps
	// the opposite var. Together the two tests prove each layer independently.
	origGuard := guardHost
	guardHost = func(string) error { return nil }
	t.Cleanup(func() { guardHost = origGuard })

	e := NewExecutor(Config{
		BaseURL:    "http://169.254.169.254",
		Auth:       Auth{Type: "none"},
		Operations: []Operation{{Name: "go", Method: "GET", Path: "/latest/meta-data"}},
	}, "")
	_, err := e.Call(context.Background(), "go", nil)
	require.Error(t, err, "the cloud metadata endpoint must never be reachable")
}

// TestBuildRequest_ReguardsResolvedHostForUnvalidatedConfig proves the
// restored guardHost pre-check fires on its own — not the dial-time guard
// inside newHTTPClient(), which this test disarms by swapping in a plain
// client with a short timeout. NewExecutor accepts a raw Config with no
// forced Validate, so a hand-built, unvalidated Operation.Path can move the
// URL's authority boundary: a Path with no leading "/" — exactly what
// Validate would refuse — parses as "@169.254.169.254/latest" appended to
// "https://api.example.test", giving userinfo "api.example.test" and HOST
// "169.254.169.254". Only guardHost catches that divergence; if it didn't
// fire, this call would either hang or dial the real (fake) baseURL host,
// never touching the metadata address, so the assertion on the error's
// content also proves it came back before any dial was attempted.
func TestBuildRequest_ReguardsResolvedHostForUnvalidatedConfig(t *testing.T) {
	orig := newHTTPClient
	newHTTPClient = func() *http.Client { return &http.Client{Timeout: 5 * time.Second} }
	t.Cleanup(func() { newHTTPClient = orig })
	// guardHost is left real (not swapped) — this test is exactly what
	// proves it, unlike TestCall_RefusesGuardedDestination which proves the
	// dial-time guard.

	e := NewExecutor(Config{
		BaseURL: "https://api.example.test",
		Auth:    Auth{Type: "none"},
		Operations: []Operation{{
			Name:   "go",
			Method: "GET",
			// No leading "/" — Validate would refuse this; NewExecutor does
			// not force Validate, so it reaches buildRequest as-is.
			Path: "@169.254.169.254/latest",
		}},
	}, "")
	_, err := e.Call(context.Background(), "go", nil)
	require.Error(t, err, "an unvalidated Path that moves the authority boundary must still be refused")
	assert.Contains(t, err.Error(), "go", "the error names the operation")
	assert.Contains(t, err.Error(), "safehttp", "the error comes from the resolved-host guard, not a dial timeout")
}

func TestCall_CapsResponseBody(t *testing.T) {
	useTestClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	t.Cleanup(srv.Close)

	e := NewExecutor(cfgFor(srv, Auth{Type: "none"}, Operation{Name: "go", Method: "GET", Path: "/x"}), "")
	e.MaxResponseBytes = 100
	res, err := e.Call(context.Background(), "go", nil)
	require.NoError(t, err)
	marker := "\n[truncated at 100 bytes]"
	assert.Equal(t, 100+len(marker), len(res.Body), "the body is capped, not the whole 4096, plus the truncation marker")
	assert.True(t, strings.HasSuffix(string(res.Body), marker), "a truncated body must carry a marker so the model can tell it was cut, not upstream-malformed")
}

// TestCall_RedactsCredentialFromTransportError proves MAJOR 1: http.Client.Do
// errors are *url.Error, which embeds the full request URL — and for
// auth.type "query" the credential IS in that URL. Go's own redaction covers
// only userinfo, never the query. ".invalid" (RFC 2606) never resolves, so
// the dial fails fast with no real network dependency; the swapped plain
// client (no SSRF guard) is used so the DNS failure — not a guard refusal —
// is what produces the error this test inspects.
func TestCall_RedactsCredentialFromTransportError(t *testing.T) {
	useTestClient(t)
	e := NewExecutor(Config{
		BaseURL:    "https://does-not-resolve.invalid",
		Auth:       Auth{Type: "query", EnvVar: "T", Name: "api_key"},
		Operations: []Operation{{Name: "go", Method: "GET", Path: "/x"}},
	}, "SEKRET-XYZ")
	_, err := e.Call(context.Background(), "go", nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SEKRET-XYZ", "the credential must never reach a tool result")
	assert.Contains(t, err.Error(), "[redacted]")
}

// TestCall_RefusesCrossHostRedirect proves MAJOR 2: Go strips only
// Authorization/Cookie on a cross-host redirect, so a header-auth credential
// (and, for query auth, the Referer) would otherwise travel to whatever host
// a redirect names. One real server plays both roles: baseURL addresses it
// as "127.0.0.1", and the "/cross" handler redirects to the SAME port under
// "localhost" — a different Hostname() string, which is all the same-host
// check compares. Because CheckRedirect fires before the second request is
// ever issued, "/x" — reachable only via that redirect — proves it was never
// hit, which is what stands in for "the credential never reached the other
// host": there's no address a real second attacker-controlled server here
// would add. A same-host redirect ("/same-from" -> "/same-to", no host in
// the Location at all) is exercised too, to prove the wrapper doesn't break
// the ordinary case.
func TestCall_RefusesCrossHostRedirect(t *testing.T) {
	useTestClient(t)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port

	var xHit, xSawCredential bool
	mux := http.NewServeMux()
	mux.HandleFunc("/cross", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://localhost:%d/x", port), http.StatusFound)
	})
	mux.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {
		xHit = true
		xSawCredential = r.Header.Get("X-Api-Key") != ""
		w.WriteHeader(200)
	})
	mux.HandleFunc("/same-from", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/same-to", http.StatusFound)
	})
	mux.HandleFunc("/same-to", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})

	srv := httptest.NewUnstartedServer(mux)
	require.NoError(t, srv.Listener.Close())
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)

	auth := Auth{Type: "header", EnvVar: "T", Name: "X-Api-Key"}

	crossOp := Operation{Name: "cross", Method: "GET", Path: "/cross"}
	eCross := NewExecutor(cfgFor(srv, auth, crossOp), "sekret")
	_, err = eCross.Call(context.Background(), "cross", nil)
	require.Error(t, err, "a cross-host redirect must be refused")
	assert.Contains(t, err.Error(), "refusing redirect off")
	assert.False(t, xHit, "the redirect target must never even be reached")
	assert.False(t, xSawCredential, "the credential must never travel off-host")

	sameOp := Operation{Name: "same", Method: "GET", Path: "/same-from"}
	eSame := NewExecutor(cfgFor(srv, auth, sameOp), "sekret")
	res, err := eSame.Call(context.Background(), "same", nil)
	require.NoError(t, err, "a same-host redirect must still succeed")
	assert.Equal(t, 200, res.Status)
}
