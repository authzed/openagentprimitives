package identityd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	fakeidpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// newCLIFixture builds a Server wired with the fake IdP kind, mirroring
// the idp-test fixture helpers. The returned server has the CLI routes
// registered automatically via NewServer.
func newCLIFixture(t *testing.T) *Server {
	t.Helper()
	scheme := idpLoaderScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaultCR("fake"), defaultSecret()).Build()
	return NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New(signerKey),
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators:  map[string]channelkinds.WebAuthenticator{},
	})
}

// doCLILogin fires GET /cli/login?state=<state>&port=<port> and returns the recorder.
func doCLILogin(t *testing.T, srv *Server, state, port string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	if port != "" {
		q.Set("port", port)
	}
	req := httptest.NewRequest(http.MethodGet, "/cli/login?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doCLIExchange fires POST /cli/exchange with the given code and state.
func doCLIExchange(t *testing.T, srv *Server, code, state string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"code": code, "state": state})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/cli/exchange", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestCLILogin_Happy verifies that GET /cli/login with valid params
// redirects to the IdP's authorize URL and stores the state token.
func TestCLILogin_Happy(t *testing.T) {
	srv := newCLIFixture(t)
	rec := doCLILogin(t, srv, "my-cli-state", "54321")
	require.Equal(t, http.StatusFound, rec.Code)
	loc := rec.Header().Get("Location")
	// fakekind.Begin returns "<issuer>/authorize?state=<stateTok>"; issuer is
	// empty in defaultCR so the URL starts with "/authorize?".
	assert.Contains(t, loc, "authorize?state=", "must redirect to IdP authorize URL, got %q", loc)
	// Verify the state token is in the store.
	rawState, err := url.ParseQuery(strings.TrimPrefix(strings.SplitN(loc, "?", 2)[1], ""))
	require.NoError(t, err)
	stateTok := rawState.Get("state")
	require.NotEmpty(t, stateTok, "IdP authorize URL must carry a state token")
	// The binding rides on the response the begin handler just wrote; the browser
	// that started the flow is the only one that can spend the state.
	bindingCookie := findCookie(rec, loginBindingCookie)
	require.NotNil(t, bindingCookie, "cli login must bind the flow to this browser")
	_, gotNext, refusal := srv.stateStore.Consume(stateTok, bindingCookie.Value, "idp")
	require.Equal(t, stateAccepted, refusal, "state token must be present in store")
	assert.Equal(t, "cli|54321|my-cli-state", gotNext, "next must encode the CLI marker")
}

// TestCLILogin_ValidationErrors covers bad port / empty state → 400.
func TestCLILogin_ValidationErrors(t *testing.T) {
	cases := []struct {
		name        string
		state       string
		port        string
		wantStatus  int
		wantBodySub string
	}{
		{
			name:        "missing state → 400",
			state:       "",
			port:        "54321",
			wantStatus:  http.StatusBadRequest,
			wantBodySub: "state",
		},
		{
			name:        "port too low → 400",
			state:       "cli-state",
			port:        "80",
			wantStatus:  http.StatusBadRequest,
			wantBodySub: "port",
		},
		{
			name:        "port too high → 400",
			state:       "cli-state",
			port:        "99999",
			wantStatus:  http.StatusBadRequest,
			wantBodySub: "port",
		},
		{
			name:        "port is junk → 400",
			state:       "cli-state",
			port:        "not-a-port",
			wantStatus:  http.StatusBadRequest,
			wantBodySub: "port",
		},
		{
			name:        "port is 0 → 400",
			state:       "cli-state",
			port:        "0",
			wantStatus:  http.StatusBadRequest,
			wantBodySub: "port",
		},
		{
			name:        "port is 1023 (below range) → 400",
			state:       "cli-state",
			port:        "1023",
			wantStatus:  http.StatusBadRequest,
			wantBodySub: "port",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCLIFixture(t)
			rec := doCLILogin(t, srv, tc.state, tc.port)
			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantBodySub)
		})
	}
}

// TestCLILogin_NoIdP verifies that a missing ClusterIdentityProvider returns
// 503 with the oap idp setup message.
func TestCLILogin_NoIdP(t *testing.T) {
	scheme := idpLoaderScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // no CR
	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New(signerKey),
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators:  map[string]channelkinds.WebAuthenticator{},
	})

	rec := doCLILogin(t, srv, "my-state", "54321")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "oap idp setup")
}

// TestCLIFullFlow exercises the entire CLI login → idp callback → loopback
// redirect → exchange sequence.
func TestCLIFullFlow(t *testing.T) {
	const (
		email       = "alice@example.com"
		displayName = "Alice"
		port        = "54321"
		cliState    = "my-cli-state-abc"
	)
	srv := newCLIFixture(t)

	// Step 1: GET /cli/login — obtain state token for IdP flow.
	fakeidpkind.NextPrincipal = identity.IdPUser(email, true, displayName)
	t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })

	loginRec := doCLILogin(t, srv, cliState, port)
	require.Equal(t, http.StatusFound, loginRec.Code, "login must redirect to IdP")
	loc := loginRec.Header().Get("Location")

	// Extract the state token from the IdP authorize URL.
	parts := strings.SplitN(loc, "?", 2)
	require.Len(t, parts, 2, "authorize URL must have query string")
	qs, err := url.ParseQuery(parts[1])
	require.NoError(t, err)
	stateTok := qs.Get("state")
	require.NotEmpty(t, stateTok, "authorize URL must carry a state token")

	// Step 2: GET /oidc/callback/idp — IdP completes authentication.
	// The binding cookie the begin handler just set: this browser started the
	// flow, so this browser may finish it.
	callbackRec := doIdPCallbackWithBinding(t, srv, "fake-code", stateTok, bindingValue(t, loginRec))
	require.Equal(t, http.StatusFound, callbackRec.Code, "IdP callback must redirect")

	// The callback must NOT set a browser cookie.
	assert.Nil(t, findCookie(callbackRec, cookieName), "CLI flow must NOT set idd_session cookie")

	// The Location must be the loopback URL with code + state.
	callbackLoc := callbackRec.Header().Get("Location")
	assert.True(t, strings.HasPrefix(callbackLoc, "http://127.0.0.1:"+port+"/callback?"),
		"loopback redirect must point at CLI port, got %q", callbackLoc)

	callbackParsed, err := url.Parse(callbackLoc)
	require.NoError(t, err)
	code := callbackParsed.Query().Get("code")
	gotState := callbackParsed.Query().Get("state")
	require.NotEmpty(t, code, "loopback redirect must carry code")
	assert.Equal(t, cliState, gotState, "loopback redirect must carry the original CLI state")

	// Step 3: POST /cli/exchange — CLI exchanges code for assertion.
	exchangeRec := doCLIExchange(t, srv, code, cliState)
	require.Equal(t, http.StatusOK, exchangeRec.Code, "exchange must succeed")
	assert.Equal(t, "application/json", exchangeRec.Header().Get("Content-Type"))

	var resp cliExchangeResponse
	require.NoError(t, json.Unmarshal(exchangeRec.Body.Bytes(), &resp))

	// Verify the assertion using the same signer key.
	signer := passthroughlink.New(signerKey)
	payload, err := signer.Verify(resp.Assertion, passthroughlink.WithExpectedAudience(passthroughlink.AudienceCLIIdentity))
	require.NoError(t, err, "assertion must verify")
	assert.Equal(t, identity.Subject("user:YWxpY2VAZXhhbXBsZS5jb20"), payload.Subject,
		"assertion subject must be the canonical user subject")
	assert.True(t, payload.SubjectVerified, "SubjectVerified must be true")
	assert.Equal(t, passthroughlink.PurposeCLIIdentity, payload.Purpose)
	// exp ≈ now + 12h (defaultIdPSessionTTL); allow generous drift.
	approxExp := time.Now().Add(12 * time.Hour)
	assert.InDelta(t, approxExp.Unix(), payload.ExpiresAt, 60, "assertion exp must be ≈now+12h")

	// Response JSON fields. Subject is the UNprefixed canonical (the
	// signed assertion carries the prefixed form, asserted above).
	assert.Equal(t, identity.CanonicalFromTrusted("YWxpY2VAZXhhbXBsZS5jb20", "test fixture"), resp.Subject)
	assert.Equal(t, email, resp.Email)
	assert.Equal(t, displayName, resp.DisplayName)
	assert.Equal(t, payload.ExpiresAt, resp.ExpiresAt)
}

// TestCLIExchange_SingleUse verifies that a code cannot be exchanged twice.
func TestCLIExchange_SingleUse(t *testing.T) {
	const (
		email    = "alice@example.com"
		port     = "54321"
		cliState = "single-use-state"
	)
	srv := newCLIFixture(t)
	fakeidpkind.NextPrincipal = identity.IdPUser(email, true, "Alice")
	t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })

	code := cliFlowGetCode(t, srv, cliState, port)

	// First exchange: success.
	rec1 := doCLIExchange(t, srv, code, cliState)
	require.Equal(t, http.StatusOK, rec1.Code, "first exchange must succeed")

	// Second exchange with same code: 403.
	rec2 := doCLIExchange(t, srv, code, cliState)
	assert.Equal(t, http.StatusForbidden, rec2.Code, "second exchange must be rejected")
	assert.Contains(t, rec2.Body.String(), "invalid or expired code")
}

// TestCLIExchange_Expired verifies that an expired code returns 403.
func TestCLIExchange_Expired(t *testing.T) {
	const cliState = "expired-state"
	srv := newCLIFixture(t)

	// Inject a past `now` so the code is immediately expired.
	past := time.Now().Add(-2 * cliCodeTTL)
	srv.cliCodes.now = func() time.Time { return past }

	code, err := srv.cliCodes.Create(identity.IdPUser("bob@example.com", true, "Bob"), 12*time.Hour, cliState)
	require.NoError(t, err)

	// Restore real now so Consume sees the entry as expired.
	srv.cliCodes.now = time.Now

	rec := doCLIExchange(t, srv, code, cliState)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid or expired code")
}

// TestCLIExchange_StateMismatch verifies that a wrong state returns 403.
func TestCLIExchange_StateMismatch(t *testing.T) {
	const cliState = "correct-state"
	srv := newCLIFixture(t)
	fakeidpkind.NextPrincipal = identity.IdPUser("alice@example.com", true, "Alice")
	t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })

	code := cliFlowGetCode(t, srv, cliState, "54321")

	rec := doCLIExchange(t, srv, code, "wrong-state")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid or expired code")
}

// TestCLIExchange_MalformedBody covers JSON decode errors and unknown fields.
func TestCLIExchange_MalformedBody(t *testing.T) {
	srv := newCLIFixture(t)

	cases := []struct {
		name string
		body string
	}{
		{name: "not JSON", body: "not json"},
		{name: "unknown field", body: `{"code":"c","state":"s","unknown":"field"}`},
		{name: "empty body", body: ""},
		{name: "truncated JSON", body: `{"code":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/cli/exchange", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "malformed body must be rejected")
		})
	}
}

// TestCLIExchange_MissingFields covers the case where required JSON fields
// are present but empty.
func TestCLIExchange_MissingFields(t *testing.T) {
	srv := newCLIFixture(t)

	cases := []struct {
		name string
		body string
	}{
		{name: "missing code", body: `{"state":"s"}`},
		{name: "missing state", body: `{"code":"c"}`},
		{name: "both empty string", body: `{"code":"","state":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/cli/exchange", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "missing required fields must be rejected")
		})
	}
}

// TestCLICallback_PolicyDenialNoLoopback verifies that a user whose email
// domain is not in the allowed list still gets 403 and no loopback redirect.
func TestCLICallback_PolicyDenialNoLoopback(t *testing.T) {
	const (
		port     = "54321"
		cliState = "policy-denial-state"
	)
	srv := newCLIFixture(t)

	// Prime the IdP state store to simulate a CLI login already in flight.
	next := "cli|" + port + "|" + cliState
	stateTok, err := srv.stateStore.NewStateWithNext("", next, testLoginBinding, "idp")
	require.NoError(t, err)

	// Use an email whose domain is NOT in defaultCR's AllowedEmailDomains ("example.com").
	fakeidpkind.NextPrincipal = identity.IdPUser("bob@evil.example", true, "Bob")
	t.Cleanup(func() { fakeidpkind.NextPrincipal = identity.Principal{} })

	rec := doIdPCallback(t, srv, "fake-code", stateTok)
	assert.Equal(t, http.StatusForbidden, rec.Code, "policy denial must return 403")
	assert.NotContains(t, rec.Header().Get("Location"), "127.0.0.1",
		"policy-denied CLI flow must NOT produce a loopback redirect")
}

// TestParseCLINext_Table covers all branches of parseCLINext.
func TestParseCLINext_Table(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantPortStr string
		wantState   string
		wantOK      bool
	}{
		{
			name:        "valid: port 1024, non-empty state",
			input:       "cli|1024|abc",
			wantPortStr: "1024",
			wantState:   "abc",
			wantOK:      true,
		},
		{
			name:        "valid: port 54321, longer state",
			input:       "cli|54321|my-state-xyz",
			wantPortStr: "54321",
			wantState:   "my-state-xyz",
			wantOK:      true,
		},
		{
			name:        "valid: port 65535 (max)",
			input:       "cli|65535|s",
			wantPortStr: "65535",
			wantState:   "s",
			wantOK:      true,
		},
		{
			name:   "missing cli prefix",
			input:  "notcli|54321|state",
			wantOK: false,
		},
		{
			name:   "missing second separator",
			input:  "cli|54321",
			wantOK: false,
		},
		{
			name:   "port too low (80)",
			input:  "cli|80|state",
			wantOK: false,
		},
		{
			name:   "port too low (1023)",
			input:  "cli|1023|state",
			wantOK: false,
		},
		{
			name:   "port too high (65536)",
			input:  "cli|65536|state",
			wantOK: false,
		},
		{
			name:   "port is junk",
			input:  "cli|notaport|state",
			wantOK: false,
		},
		{
			name:   "empty state",
			input:  "cli|54321|",
			wantOK: false,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "valid safe-next path (not a CLI marker)",
			input:  "/artifacts/v/abc",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			portStr, state, ok := parseCLINext(tc.input)
			assert.Equal(t, tc.wantOK, ok, "parseCLINext(%q) ok", tc.input)
			if tc.wantOK {
				assert.Equal(t, tc.wantPortStr, portStr)
				assert.Equal(t, tc.wantState, state)
			}
		})
	}
}

// cliFlowGetCode is a helper that drives the full CLI flow up to code
// issuance (login → idp callback) and returns the one-time code.
// fakeidpkind.NextPrincipal must be set by the caller.
func cliFlowGetCode(t *testing.T, srv *Server, cliState, port string) string {
	t.Helper()

	loginRec := doCLILogin(t, srv, cliState, port)
	require.Equal(t, http.StatusFound, loginRec.Code, "login must redirect to IdP")

	loc := loginRec.Header().Get("Location")
	parts := strings.SplitN(loc, "?", 2)
	require.Len(t, parts, 2)
	qs, err := url.ParseQuery(parts[1])
	require.NoError(t, err)
	stateTok := qs.Get("state")
	require.NotEmpty(t, stateTok)

	cbRec := doIdPCallbackWithBinding(t, srv, "fake-code", stateTok, bindingValue(t, loginRec))
	require.Equal(t, http.StatusFound, cbRec.Code, "IdP callback must redirect")

	cbLoc := cbRec.Header().Get("Location")
	cbParsed, err := url.Parse(cbLoc)
	require.NoError(t, err)
	code := cbParsed.Query().Get("code")
	require.NotEmpty(t, code, "callback must carry a code")
	return code
}
