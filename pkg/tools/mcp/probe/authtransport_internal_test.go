package probe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testEndpoint = "https://mcp.invalid/rpc"

// stubRoundTripper replays a scripted sequence of exchanges, one per call.
type stubRoundTripper struct {
	steps []func() (*http.Response, error)
	n     int
}

func (s *stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	if s.n >= len(s.steps) {
		return nil, errors.New("stub: no step scripted for this call")
	}
	step := s.steps[s.n]
	s.n++
	return step()
}

func respond(status int, body string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
		}, nil
	}
}

func transportFailure(msg string) func() (*http.Response, error) {
	return func() (*http.Response, error) { return nil, errors.New(msg) }
}

// pinnedTransport builds a transport scoped to testEndpoint, the way withAuth
// builds one in production.
func pinnedTransport(t *testing.T, base http.RoundTripper) *authTransport {
	t.Helper()
	o, err := originOf(testEndpoint)
	require.NoError(t, err)
	return &authTransport{base: base, endpoint: o}
}

// exchangeRun drives one JSON-RPC-carrying POST — the request that IS the
// logical MCP exchange — attributing it to ex the way the go-sdk's per-call
// context does in production.
func exchangeRun(t *testing.T, at *authTransport, ex *exchange) {
	t.Helper()
	exchangeMethod(t, at, ex, http.MethodPost)
}

// exchangeMethod drives one round trip with an arbitrary method, so a test can
// send the SIDE requests (teardown DELETE, standalone-SSE GET) the SDK issues
// around the POST.
func exchangeMethod(t *testing.T, at *authTransport, ex *exchange, method string) {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{}`)
	}
	req, err := http.NewRequest(method, testEndpoint, body)
	require.NoError(t, err)
	if ex != nil {
		req = req.WithContext(context.WithValue(req.Context(), exchangeCtxKey{}, ex))
	}
	resp, rtErr := at.RoundTrip(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	_ = rtErr // the caller asserts on what the transport RECORDED, not on this
}

// TestAuthTransportStatusDescribesOnlyTheCurrentExchange pins that the recorded
// status is scoped to one exchange, not to the transport's whole lifetime.
//
// This matters beyond error labeling: pkg/agent/tool/mcp turns the recorded
// status into agenttool.Result.HTTPStatus, which the credential-update
// corroboration path (pkg/agent/tool/authfail) reads as the platform's OWN
// evidence that a credential was rejected. A status carried over from an
// earlier exchange would fabricate that evidence.
func TestAuthTransportStatusDescribesOnlyTheCurrentExchange(t *testing.T) {
	underlying := errors.New("dial tcp: connection refused")

	t.Run("401 then SUCCESS then transport failure: no status, not a stale 401", func(t *testing.T) {
		at := pinnedTransport(t, &stubRoundTripper{steps: []func() (*http.Response, error){
			respond(http.StatusUnauthorized, `{"error":"token expired"}`),
			respond(http.StatusOK, `{"result":{}}`),
			transportFailure("dial tcp: connection refused"),
		}})

		exchangeRun(t, at, &exchange{}) // 401 — recorded against ITS exchange
		exchangeRun(t, at, &exchange{}) // 200
		third := &exchange{}
		exchangeRun(t, at, third) // transport-level failure — no response, no status

		got := third.httpError(underlying)
		var he *HTTPError
		require.False(t, errors.As(got, &he),
			"a transport failure that never received a response must not surface as an HTTP error")
		assert.Equal(t, underlying, got, "the original error must pass through unchanged")
	})

	t.Run("401 then transport failure (no success between): still no stale status", func(t *testing.T) {
		at := pinnedTransport(t, &stubRoundTripper{steps: []func() (*http.Response, error){
			respond(http.StatusUnauthorized, `{"error":"token expired"}`),
			transportFailure("dial tcp: connection refused"),
		}})

		exchangeRun(t, at, &exchange{})
		second := &exchange{}
		exchangeRun(t, at, second)

		var he *HTTPError
		assert.False(t, errors.As(second.httpError(underlying), &he),
			"the 401 belonged to the previous exchange; only the CURRENT one may set a status")
	})

	t.Run("401 on the current exchange still surfaces as a typed auth error", func(t *testing.T) {
		at := pinnedTransport(t, &stubRoundTripper{steps: []func() (*http.Response, error){
			respond(http.StatusOK, `{"result":{}}`),
			respond(http.StatusUnauthorized, `{"error":"token expired"}`),
		}})

		exchangeRun(t, at, &exchange{}) // 200
		current := &exchange{}
		exchangeRun(t, at, current) // 401 — this one IS current

		var he *HTTPError
		require.True(t, errors.As(current.httpError(underlying), &he),
			"per-exchange scoping must not stop a REAL auth failure from surfacing")
		assert.Equal(t, http.StatusUnauthorized, he.StatusCode)
		assert.True(t, he.IsAuth())
		assert.Contains(t, he.Body, "token expired")
	})

	t.Run("a benign SIDE request must not erase the current exchange's status", func(t *testing.T) {
		// One logical exchange is several round trips. A teardown DELETE or a
		// standalone-SSE GET arriving after the failing POST must neither
		// record nor erase — even when it is attributed to the same exchange.
		at := pinnedTransport(t, &stubRoundTripper{steps: []func() (*http.Response, error){
			respond(http.StatusUnauthorized, `{"error":"token expired"}`),
			respond(http.StatusBadRequest, `session not found`), // teardown DELETE
		}})

		ex := &exchange{}
		exchangeRun(t, at, ex) // POST — records the 401
		exchangeMethod(t, at, ex, http.MethodDelete)

		var he *HTTPError
		require.True(t, errors.As(ex.httpError(underlying), &he),
			"a side request that carries no JSON-RPC must not erase the failure the POST recorded")
		assert.Equal(t, http.StatusUnauthorized, he.StatusCode)
		assert.True(t, he.IsAuth())
	})

	t.Run("5xx on the current exchange surfaces as a non-auth HTTP error", func(t *testing.T) {
		at := pinnedTransport(t, &stubRoundTripper{steps: []func() (*http.Response, error){
			respond(http.StatusUnauthorized, `{"error":"token expired"}`),
			respond(http.StatusBadGateway, `upstream down`),
		}})

		exchangeRun(t, at, &exchange{}) // 401
		current := &exchange{}
		exchangeRun(t, at, current) // 502

		var he *HTTPError
		require.True(t, errors.As(current.httpError(underlying), &he))
		assert.Equal(t, http.StatusBadGateway, he.StatusCode,
			"the CURRENT exchange's status wins; a stale 401 must not shadow it")
		assert.False(t, he.IsAuth(),
			"mislabeling a server outage as an auth failure is what would send a human to re-enter a working token")
	})
}

// TestAuthTransportStatusIsNotSharedBetweenConcurrentExchanges is the
// SessionCache shape: ONE transport, many in-flight calls. Call B's 401 must
// not attach itself to call A's opaque transport failure.
//
// The consequence of getting this wrong is not a mislabelled log line. The
// fabricated HTTPError becomes agenttool.Result.HTTPStatus, which the
// credential-update path reads as the platform's own evidence that the
// credential was REJECTED — so a call that received no auth rejection at all
// manufactures corroboration for revoking a working credential.
func TestAuthTransportStatusIsNotSharedBetweenConcurrentExchanges(t *testing.T) {
	// Order the interleaving deterministically: A's POST is dispatched first
	// and blocks; B's POST then completes with a 401; only then does A fail.
	bDone := make(chan struct{})
	var aStarted sync.Once
	aReached := make(chan struct{})

	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-Call") == "B" {
			return respond(http.StatusUnauthorized, `{"error":"token expired"}`)()
		}
		aStarted.Do(func() { close(aReached) })
		<-bDone // A's failure lands AFTER B recorded its 401
		return nil, errors.New("dial tcp: connection refused")
	})
	at := pinnedTransport(t, base)

	exA, exB := &exchange{}, &exchange{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		driveTagged(t, at, exA, "A")
	}()
	<-aReached
	driveTagged(t, at, exB, "B")
	close(bDone)
	wg.Wait()

	var heB *HTTPError
	require.True(t, errors.As(exB.httpError(errors.New("b")), &heB),
		"B genuinely received a 401; it must still surface")
	assert.Equal(t, http.StatusUnauthorized, heB.StatusCode)

	underlying := errors.New("dial tcp: connection refused")
	var heA *HTTPError
	assert.False(t, errors.As(exA.httpError(underlying), &heA),
		"A received no HTTP response at all; a concurrent call's 401 must not be attributed to it")
	assert.Equal(t, underlying, exA.httpError(underlying))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// driveTagged issues one POST attributed to ex and tagged so the base transport
// can tell the two concurrent calls apart.
func driveTagged(t *testing.T, at *authTransport, ex *exchange, tag string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, testEndpoint, strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("X-Call", tag)
	req = req.WithContext(context.WithValue(req.Context(), exchangeCtxKey{}, ex))
	resp, _ := at.RoundTrip(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// TestAuthTransportWithholdsCredentialOffEndpoint is the redirect guard.
//
// The header is injected inside RoundTrip, below the layer where net/http
// snapshots the original request's headers (makeHeadersCopier runs once, before
// the redirect loop), so Go's cross-host Authorization stripping has nothing to
// strip and every hop is re-authenticated by us. An open redirect on an
// otherwise-honest MCP host — or on a CDN in front of it — then escalates to
// full disclosure of the enterprise OAuth/PAT credential.
func TestAuthTransportWithholdsCredentialOffEndpoint(t *testing.T) {
	var attackerSawAuth atomic.Value
	attackerSawAuth.Store("")
	var reauthCalls atomic.Int32

	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerSawAuth.Store(r.Header.Get("Authorization"))
		// A 401 here must NOT make us mint a fresh token and hand it over.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(attacker.Close)

	var honestSawAuth atomic.Value
	honestSawAuth.Store("")
	honest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, attacker.URL+"/steal", http.StatusFound)
		default:
			honestSawAuth.Store(r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(honest.Close)

	hc, _, err := withAuth(&http.Client{}, honest.URL, "Authorization", "Bearer secret-upstream-token",
		func(context.Context) (string, string, error) {
			reauthCalls.Add(1)
			return "Authorization", "Bearer freshly-minted-token", nil
		})
	require.NoError(t, err)

	resp, err := hc.Get(honest.URL + "/redirect")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Empty(t, attackerSawAuth.Load(),
		"the credential is minted for the MCP endpoint; a redirect must not deliver it to another origin")
	assert.Zero(t, reauthCalls.Load(),
		"a 401 from an off-endpoint hop is not a rejection of our credential; minting a fresh one would hand THAT over too")
}

// TestAuthTransportReauthsOnlyOnThePOST is the side-request half of the reauth
// guard, on-endpoint. TestAuthTransportWithholdsCredentialOffEndpoint covers the
// !onEndpoint clause; this covers the `req.Method != http.MethodPost` clause,
// which is the ONLY thing excluding a side request that shares the endpoint's
// origin.
//
// The go-sdk issues a standalone-SSE GET (inside Connect) and a teardown DELETE,
// both to the endpoint itself, so both pass the origin pin. A 401 on either is
// the server talking about a side request, not a rejection of the caller's
// credential. Left to reauth, a hostile server answering 200/initialize and
// 401/side-request drove reauth → reauthPersist → setAuthLocked on every session
// open — for a minted credential (ID-JAG, a GitHub App) a fresh IdP token
// exchange each time. So a non-POST 401 must NOT reauth; only the JSON-RPC POST
// does.
//
// Deleting the `req.Method != http.MethodPost` clause makes the GET/DELETE rows
// reauth (reauthCalls==1) and fails this test — which the off-endpoint test does
// not, because its 401 is already excluded by !onEndpoint.
func TestAuthTransportReauthsOnlyOnThePOST(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		wantReauth bool
	}{
		{name: "on-endpoint JSON-RPC POST 401 is the caller's rejected credential: reauths", method: http.MethodPost, wantReauth: true},
		{name: "on-endpoint standalone-SSE GET 401 is a side request: does NOT reauth", method: http.MethodGet, wantReauth: false},
		{name: "on-endpoint teardown DELETE 401 is a side request: does NOT reauth", method: http.MethodDelete, wantReauth: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reauthCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Every method, every path: 401. The only variable under test is
				// the request METHOD, and whether it provokes a reauth.
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}))
			t.Cleanup(srv.Close)

			hc, _, err := withAuth(&http.Client{}, srv.URL, "Authorization", "Bearer secret-upstream-token",
				func(context.Context) (string, string, error) {
					reauthCalls.Add(1)
					return "Authorization", "Bearer freshly-minted-token", nil
				})
			require.NoError(t, err)

			req, err := http.NewRequest(tc.method, srv.URL, nil)
			require.NoError(t, err)
			resp, err := hc.Do(req)
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })

			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the 401 must pass through in every case")
			if tc.wantReauth {
				assert.Positive(t, reauthCalls.Load(),
					"a 401 on the on-endpoint JSON-RPC POST is the caller's rejected credential and must reauth")
			} else {
				assert.Zero(t, reauthCalls.Load(),
					"a 401 on an on-endpoint side request (SSE GET / teardown DELETE) is not the caller's "+
						"credential; reauthing would mint a fresh token for a hostile server to provoke")
			}
		})
	}
}

// TestAuthTransportKeepsCredentialOnSameOriginRedirect is the other half: the
// pin must not break the ordinary same-host redirect (a trailing-slash or
// path-normalizing hop), which would turn a working MCP server into a 401.
func TestAuthTransportKeepsCredentialOnSameOriginRedirect(t *testing.T) {
	var sawAuth atomic.Value
	sawAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			http.Redirect(w, r, "/mcp/", http.StatusFound)
			return
		}
		sawAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	hc, _, err := withAuth(&http.Client{}, srv.URL+"/mcp", "Authorization", "Bearer secret-upstream-token", nil)
	require.NoError(t, err)
	resp, err := hc.Get(srv.URL + "/mcp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, "Bearer secret-upstream-token", sawAuth.Load(),
		"a redirect within the SAME origin is the normal case and must still carry the credential")
}

// TestWithAuthRefusesEndpointWithNoOrigin: an endpoint with no scheme+host has
// no origin to scope the credential to. Fail closed and loudly rather than
// install an unpinned transport, which is the leak itself.
func TestWithAuthRefusesEndpointWithNoOrigin(t *testing.T) {
	for _, endpoint := range []string{"", "mcp.invalid/rpc", "/rpc"} {
		t.Run("endpoint "+strconv.Quote(endpoint)+": withAuth errors, no client returned", func(t *testing.T) {
			hc, _, err := withAuth(&http.Client{}, endpoint, "Authorization", "Bearer secret", nil)
			require.Error(t, err)
			assert.Nil(t, hc)
		})
	}
}

// TestOriginOfURLCanonicalizes pins the comparison the pin is made of: case and
// a default port must not split one origin into two (which would withhold the
// credential from the real endpoint), and a NON-default port must not be folded
// away (which would send it to a different service on the same host).
func TestOriginOfURLCanonicalizes(t *testing.T) {
	cases := []struct {
		name       string
		endpoint   string
		target     string
		wantSameAs bool
	}{
		{"uppercase host of the endpoint: same origin, credential still sent", "https://mcp.invalid/rpc", "https://MCP.INVALID/rpc", true},
		{"explicit default port: same origin, credential still sent", "https://mcp.invalid/rpc", "https://mcp.invalid:443/rpc", true},
		{"different port on the same host: different origin, credential withheld", "https://mcp.invalid/rpc", "https://mcp.invalid:8443/rpc", false},
		{"scheme downgrade to http: different origin, credential withheld", "https://mcp.invalid/rpc", "http://mcp.invalid/rpc", false},
		{"different host: different origin, credential withheld", "https://mcp.invalid/rpc", "https://evil.invalid/rpc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := originOf(tc.endpoint)
			require.NoError(t, err)
			at := &authTransport{endpoint: o}
			u, err := url.Parse(tc.target)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSameAs, at.matchesOrigin(u))
		})
	}
}

// TestListTools401SurvivesTheSDKsTeardownRequest is the PROBE-level guard for
// the same asymmetry, driven through the public Client against a real server.
//
// It exists because pkg/tools/mcp/testing's fake cannot express the shape that breaks
// it: that fake faults EVERY request with the configured status, so its
// teardown DELETE re-records the 401 and masks the hole entirely. Here the side
// requests deliberately answer DIFFERENTLY from the RPC — 401 on POST, 400 on a
// session-less DELETE, which is what the go-sdk's own streamable server does.
//
// The sequence being defended: initialize POST 401s → mcp.Client.Connect calls
// cs.Close() → a teardown DELETE goes out through the SAME transport → and only
// then does ListTools read the recorded status. Erasing on that DELETE would
// leave nothing to read, and every consumer that branches on IsAuth() (the
// MCPServer controller's and `oap tools mcp probe`'s surfaced "HTTP 401")
// would silently degrade to an opaque connect error.
//
// It doubles as the proof that the per-call context genuinely reaches the
// go-sdk's JSON-RPC POST, which is what makes the exchange record per-call.
func TestListTools401SurvivesTheSDKsTeardownRequest(t *testing.T) {
	var sawDelete atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			// The JSON-RPC-carrying request: reject the credential.
			http.Error(w, `{"error":"token expired or is invalid"}`, http.StatusUnauthorized)
		case http.MethodDelete:
			// Session teardown for a session that was never established — the
			// go-sdk streamable server answers 400 here, NOT 401.
			sawDelete.Store(true)
			http.Error(w, "session not found", http.StatusBadRequest)
		default:
			// Standalone-SSE probe.
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)

	_, err := (&Client{HTTP: http.DefaultClient, URL: srv.URL}).
		ListTools(context.Background(), "Authorization", "Bearer stale-token")
	require.Error(t, err)

	var he *HTTPError
	require.Truef(t, errors.As(err, &he),
		"a 401 initialize must surface as a typed *HTTPError even though the SDK's teardown "+
			"request ran afterwards and answered %d; got %v", http.StatusBadRequest, err)
	assert.Equal(t, http.StatusUnauthorized, he.StatusCode)
	assert.True(t, he.IsAuth(),
		"consumers branch on IsAuth() to tell 'token expired, re-auth' from 'server unhealthy'")

	// Not an assertion about our code so much as about the fixture: if the SDK
	// ever stops issuing the teardown, this test silently stops covering the
	// regression it was written for.
	assert.True(t, sawDelete.Load(),
		"fixture no longer exercises the post-failure side request; this test would no longer catch the regression")
}
