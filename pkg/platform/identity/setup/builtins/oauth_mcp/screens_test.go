package oauth_mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	mcpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/mcp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/oauth_mcp"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"
)

// tokens the two fake servers mint. Distinct per path so a test can tell WHICH
// exchange produced the stored bundle — the difference between "the flow
// registered a client and used it" and "the flow used the credentials the user
// pasted" is invisible if both mint the same string.
const (
	dcrAccessToken   = "fake-access-token-dcr-registered"
	dcrRefreshToken  = "fake-refresh-token-dcr-registered"
	noDCRAccessToken = "fake-access-token-existing-app"
)

// fakeDCRServer advertises a registration_endpoint, so oauth.Login registers a
// client dynamically and never needs credentials from the user. /token reports
// the client_id it was called with so a test can prove the exchange used the
// REGISTERED client rather than something else.
func fakeDCRServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="mcp", resource_metadata="`+srv.URL+`/resource-meta"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/resource-meta", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_servers": []string{srv.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"registration_endpoint":  srv.URL + "/register",
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":     "dcr-issued-client",
			"client_secret": "dcr-issued-secret",
		})
	})
	mux.HandleFunc("/authorize", redirectWithCode)
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  dcrAccessToken,
			"refresh_token": dcrRefreshToken,
			"token_type":    "Bearer",
			"expires_in":    7200,
			"scope":         "read write",
		})
	})
	srv.Start()
	return srv
}

// redirectWithCode is the /authorize handler both fakes share: bounce straight
// back to the loopback callback with a code and the caller's state.
func redirectWithCode(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	target, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	rq := target.Query()
	rq.Set("code", "fake-code")
	rq.Set("state", q.Get("state"))
	target.RawQuery = rq.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// mcpTargetFor builds the authkind Target the flow reads its server URL from.
func mcpTargetFor(t *testing.T, serverURL string) authkind.Target {
	t.Helper()
	cr := &spiceboxv1alpha1.MCPServer{
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: serverURL},
		},
	}
	cr.Name = "demo-mcp"
	return mcpkind.NewTarget(cr)
}

// runFlow drives the flow end to end the way the engine does: ask for screens,
// present them over the plain driver with a scripted stdin, then let Result
// store. Returns what reached Store, the answered State, and the run error.
func runFlow(t *testing.T, target authkind.Target, stdin string) ([]builtins.StoreValue, *tui.State, error) {
	t.Helper()

	var stored []builtins.StoreValue
	req := builtins.Request{
		Requirement:  authkind.CredentialRequirement{ProviderID: "oauth-mcp"},
		Target:       target,
		Namespace:    "demo-ns",
		IdentityName: "demo-bot",
		Store: func(_ context.Context, v builtins.StoreValue) error {
			stored = append(stored, v)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	flow := oauth_mcp.New()
	screens, err := flow.Screens(ctx, req)
	if err != nil {
		return stored, tui.NewState(), err
	}

	var out bytes.Buffer
	st, err := tui.Run(ctx, screens, tui.Options{
		Theme: tui.NewTheme(tui.Caps{}),
		In:    strings.NewReader(stdin),
		Out:   &out,
	})
	if err != nil {
		return stored, st, err
	}
	return stored, st, flow.Result(ctx, req, st)
}

// installLoopbackSeams points the flow's browser hook at a plain HTTP GET (so
// the callback fires without a real browser) and swaps the SSRF-guarded client
// for a plain one, since the fakes bind loopback addresses the guarded dialer
// correctly refuses.
func installLoopbackSeams(t *testing.T) {
	t.Helper()
	browsertest.Use(t, driveBrowser)
	oauth_mcp.SetHTTPClient(func() *http.Client { return http.DefaultClient })
	t.Cleanup(func() { oauth_mcp.SetHTTPClient(nil) })
}

// TestScreensDCRPathStoresTheRegisteredClientsBundle is the happy path: the
// server advertises Dynamic Client Registration, so the flow registers a client
// and completes without asking the user for anything.
//
// It asserts the WHOLE bundle, not just that something was stored. Every field
// here is load-bearing at refresh time — a bundle carrying the access token in
// the refresh slot, or missing the token endpoint, authenticates today and dies
// silently at the first renewal, which is exactly the failure this flow exists
// to prevent.
func TestScreensDCRPathStoresTheRegisteredClientsBundle(t *testing.T) {
	srv := fakeDCRServer(t)
	t.Cleanup(srv.Close)
	installLoopbackSeams(t)

	stored, st, err := runFlow(t, mcpTargetFor(t, srv.URL+"/mcp"), "")
	require.NoError(t, err, "a DCR-capable server needs no answers, so the run must complete unattended")
	require.Len(t, stored, 1, "exactly one credential must be stored")

	got := stored[0]
	require.NotNil(t, got.OAuth, "an oauth flow must store an OAuth bundle, not a bearer")
	assert.Equal(t, dcrAccessToken, got.OAuth.AccessToken, "stored AccessToken")
	assert.Equal(t, dcrRefreshToken, got.OAuth.RefreshToken, "stored RefreshToken")
	assert.Equal(t, 7200, got.OAuth.ExpiresIn, "stored ExpiresIn")
	assert.Equal(t, srv.URL+"/token", got.OAuth.TokenEndpoint, "stored TokenEndpoint")
	assert.Equal(t, "dcr-issued-client", got.OAuth.ClientID,
		"the bundle must carry the DYNAMICALLY REGISTERED client_id; refresh authenticates with it")
	assert.Equal(t, "dcr-issued-secret", got.OAuth.ClientSecret, "stored ClientSecret")
	assert.Equal(t, "read write", got.OAuth.Scope, "stored Scope")

	assert.Empty(t, st.Get("client_id"),
		"a DCR run must not have asked for a client_id; that question belongs to the no-DCR branch alone")
}

// TestScreensNoDCRAsksForTheClientAndUsesIt covers the pre-registered-app path:
// the server advertises no registration_endpoint, so the flow must ask for the
// user's own OAuth client and re-run the exchange with it.
//
// The redirect URI is asserted because it is not cosmetic: provider OAuth apps
// only accept a redirect_uri that was whitelisted ahead of time, so the port
// announced to the user must be the port the second attempt actually listens
// on. A flow that announced one port and bound another would send every user of
// this path to a redirect_uri mismatch.
func TestScreensNoDCRAsksForTheClientAndUsesIt(t *testing.T) {
	srv := fakeNoDCRServer(t)
	t.Cleanup(srv.Close)
	installLoopbackSeams(t)

	stored, st, err := runFlow(t, mcpTargetFor(t, srv.URL+"/mcp"), "pasted-cid\npasted-csecret\n")
	require.NoError(t, err, "the pasted client credentials must carry the flow to completion")
	require.Len(t, stored, 1, "exactly one credential must be stored")

	got := stored[0]
	require.NotNil(t, got.OAuth, "an oauth flow must store an OAuth bundle, not a bearer")
	assert.Equal(t, noDCRAccessToken, got.OAuth.AccessToken,
		"the stored token must come from the SECOND exchange, the one made with the pasted client")
	assert.Equal(t, "pasted-cid", got.OAuth.ClientID, "the bundle must carry the client the user pasted")
	assert.Equal(t, "pasted-csecret", got.OAuth.ClientSecret, "the bundle must carry the secret the user pasted")

	redirectURI := st.Get(oauth_mcp.KeyRedirectURI)
	require.NotEmpty(t, redirectURI, "the flow must record the redirect URI it announced to the user")
	assert.True(t, strings.HasPrefix(redirectURI, "http://127.0.0.1:"),
		"announced redirect URI = %q, want an http://127.0.0.1:PORT/callback loopback", redirectURI)
	assert.True(t, strings.HasSuffix(redirectURI, "/callback"),
		"announced redirect URI = %q, want a /callback path", redirectURI)
}

// TestScreensNoDCRAcceptsAPublicClient covers the OAuth client that has no
// secret at all.
//
// Public clients are not an edge case here: an app registered for a native or
// CLI redirect is public by RFC 8252's recommendation, and PKCE is what secures
// it instead of a secret. A flow that demanded one would refuse the client type
// this loopback redirect flow is specified to use.
func TestScreensNoDCRAcceptsAPublicClient(t *testing.T) {
	srv := fakeNoDCRServer(t)
	t.Cleanup(srv.Close)
	installLoopbackSeams(t)

	// A bare newline for the secret: the user pressing Enter on a field they
	// have nothing to put in.
	stored, st, err := runFlow(t, mcpTargetFor(t, srv.URL+"/mcp"), "pasted-cid\n\n")
	require.NoError(t, err, "a public client must carry the flow to completion without a secret")
	require.Len(t, stored, 1)

	require.NotNil(t, stored[0].OAuth)
	assert.Equal(t, noDCRAccessToken, stored[0].OAuth.AccessToken)
	assert.Equal(t, "pasted-cid", stored[0].OAuth.ClientID)
	assert.Empty(t, stored[0].OAuth.ClientSecret,
		"a public client has no secret, and inventing one would break the refresh grant")

	assert.True(t, st.Has(oauth_mcp.KeyClientSecret),
		"the secret question was ASKED and answered with nothing; recording nothing at all would make it "+
			"indistinguishable from a run that never reached it")
}

// credentialsIn returns the strings in v that ARE the credential — the ones
// whose disclosure is the harm — skipping any the fixture did not mint.
//
// Deliberately mirrors credentialsIn in builtins/flow_contract_test.go, field
// for field, because this test stands in for that one on this flow (see
// TestNoCredentialReachesTheSummaryOnTheOAuthFlow). Client ids, token endpoints
// and scopes are absent from both: they are not secret, and treating them as
// such would make the test refuse legitimate summary lines.
func credentialsIn(v builtins.StoreValue) []string {
	out := []string{}
	add := func(s string) {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	if v.OAuth != nil {
		add(v.OAuth.AccessToken)
		add(v.OAuth.RefreshToken)
		add(v.OAuth.ClientSecret)
	}
	return out
}

// TestNoCredentialReachesTheSummaryOnTheOAuthFlow is this flow's share of the
// registry-wide masking contract in builtins/flow_contract_test.go, which names
// this flow in flowsWithTheirOwnMaskingTest and requires this test to exist.
//
// It is asserted here rather than there because that test describes a flow with
// a bare builtins.Request and seeds every answer key from one string — neither
// of which this flow can be driven by. It needs an MCPServer target to have any
// screens at all, and its credential is MINTED by a live exchange rather than
// typed, so there is no answer key to seed it through. The property being
// defended is identical: the summary outlives the terminal, so nothing that
// reached Store may appear in it verbatim.
//
// BOTH fixtures are exercised because neither alone covers the whole bundle:
// the no-DCR server mints a client secret but no refresh token, and the DCR
// server mints a refresh token and a registration-issued secret. Run against
// only the first — as this test was when written — a note added for the REFRESH
// token would pass, which is the worst miss available here: it is the
// longer-lived of the two secrets and the one that still works tomorrow.
func TestNoCredentialReachesTheSummaryOnTheOAuthFlow(t *testing.T) {
	cases := []struct {
		name  string
		fake  func(*testing.T) *httptest.Server
		stdin string
		// want names the bundle fields this fixture actually mints, so a
		// fixture that quietly stopped minting one is reported as a gap in
		// coverage rather than passing with less to check.
		want int
	}{
		{
			name:  "dynamically registered client: neither the access nor the refresh token is summarised",
			fake:  fakeDCRServer,
			stdin: "",
			want:  3, // access + refresh + registration-issued secret
		},
		{
			name:  "client pasted by the user: neither the access token nor the pasted secret is summarised",
			fake:  fakeNoDCRServer,
			stdin: "pasted-cid\npasted-csecret\n",
			want:  2, // access + pasted secret; this server mints no refresh token
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.fake(t)
			t.Cleanup(srv.Close)
			installLoopbackSeams(t)

			stored, st, err := runFlow(t, mcpTargetFor(t, srv.URL+"/mcp"), tc.stdin)
			require.NoError(t, err)
			require.Len(t, stored, 1)

			creds := credentialsIn(stored[0])
			require.Len(t, creds, tc.want,
				"this fixture no longer mints the credentials this case exists to check: got %d of %d", len(creds), tc.want)

			notes := st.Notes()
			require.NotEmpty(t, notes, "the flow recorded no summary at all, so this test proved nothing")
			for _, n := range notes {
				for _, cred := range creds {
					assert.NotContains(t, n.Value, cred,
						"summary line %q carries a stored credential verbatim", n.Label)
					assert.NotContains(t, n.Label, cred,
						"summary label %q carries a stored credential verbatim", n.Label)
				}
			}
		})
	}
}

// TestScreensRefuseATargetItCannotAuthorize covers the two ways this flow can
// be asked for something it cannot describe. Both must be refused by Screens
// with a reason, never answered with an empty screen list: the contract every
// flow answers to is that a nil error comes with at least one screen, because a
// run that asks nothing would go on to store whatever an unanswered State held.
func TestScreensRefuseATargetItCannotAuthorize(t *testing.T) {
	cases := []struct {
		name   string
		target authkind.Target
		want   string
	}{
		{
			name:   "target is not an MCPServer: refused naming the target kind",
			target: nil,
			want:   "MCPServer",
		},
		{
			name:   "MCPServer carries no server URL: refused naming the empty field",
			target: mcpTargetFor(t, ""),
			want:   "url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			screens, err := oauth_mcp.New().Screens(context.Background(), builtins.Request{
				Target:       tc.target,
				IdentityName: "demo-bot",
			})
			require.Error(t, err, "a flow with nothing to offer must say so")
			assert.Empty(t, screens, "a flow that refuses must not also hand back screens")
			assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(tc.want))
		})
	}
}

// TestAnnouncedRedirectHostTracksTheOverride pins the half of the callback
// address that is not the port.
//
// The user is told one redirect URI and asked to register it on their OAuth
// app; oauth.Login then resolves the host again at authorize time from
// OAUTH_REDIRECT_HOST. If this flow announced a hardcoded 127.0.0.1 while Login
// sent localhost, the two would disagree for exactly the users who set that
// variable — and a provider matches redirect_uri byte for byte, so they would
// get the redirect_uri mismatch the port pinning exists to prevent.
//
// Not parallel, and not table-driven across goroutines: t.Setenv forbids both,
// and the process-global it mutates is the whole subject.
func TestAnnouncedRedirectHostTracksTheOverride(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    string
		wantErr string
	}{
		{name: "unset: the RFC 8252-preferred loopback IP", env: "", want: "127.0.0.1"},
		{name: "localhost: honoured, for providers that reject the IP form", env: "localhost", want: "localhost"},
		{name: "the IP form set explicitly: honoured", env: "127.0.0.1", want: "127.0.0.1"},
		{
			name:    "a host that is not loopback at all: refused, naming the variable that set it",
			env:     "evil.example.com",
			wantErr: "OAUTH_REDIRECT_HOST",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OAUTH_REDIRECT_HOST", tc.env)
			got, err := oauth_mcp.RedirectHostForTest()
			if tc.wantErr != "" {
				require.Error(t, err, "an unusable host must be refused before a browser opens")
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestAnnouncedRedirectURIIsTheOneSentToAuthorize is the end-to-end half of the
// same property, and the one that would actually have caught the bug: the
// address shown to the user must be the address the provider is asked to
// redirect to.
//
// It is read off the authorization URL rather than off State, because State is
// what the flow believes and the query parameter is what the provider is told.
// Asserting the flow against its own bookkeeping would pass with both halves
// wrong in the same way.
func TestAnnouncedRedirectURIIsTheOneSentToAuthorize(t *testing.T) {
	t.Setenv("OAUTH_REDIRECT_HOST", "localhost")

	srv := fakeNoDCRServer(t)
	t.Cleanup(srv.Close)
	installLoopbackSeams(t)

	// Intercept between the flow and the fake browser to read the redirect_uri
	// the provider is being handed.
	var sentToAuthorize string
	browsertest.Use(t, func(authURL string) error {
		u, perr := url.Parse(authURL)
		if perr != nil {
			return perr
		}
		sentToAuthorize = u.Query().Get("redirect_uri")
		return driveBrowser(authURL)
	})

	stored, st, err := runFlow(t, mcpTargetFor(t, srv.URL+"/mcp"), "pasted-cid\npasted-csecret\n")
	require.NoError(t, err, "the host override must not break the flow")
	require.Len(t, stored, 1)

	announced := st.Get(oauth_mcp.KeyRedirectURI)
	require.NotEmpty(t, announced)
	assert.True(t, strings.HasPrefix(announced, "http://localhost:"),
		"announced redirect URI = %q, but OAUTH_REDIRECT_HOST asked for the hostname form", announced)
	assert.Equal(t, announced, sentToAuthorize,
		"the user was told to allow %q but the provider was sent %q; a provider matches redirect_uri byte for byte",
		announced, sentToAuthorize)
}

// TestClientGuidanceFitsTheNoteWidth guards the one block in this flow a user
// has to copy out of the screen.
//
// The redirect URI has to be pasted into the provider's OAuth app allow-list
// byte for byte, and huh wraps an over-long note line — and, past the form's
// column budget, truncates it — with no sign that a second half exists. A
// wrapped redirect URI is one the provider rejects, which surfaces later as an
// opaque failure the user has no way to trace back to a rendering bug.
func TestClientGuidanceFitsTheNoteWidth(t *testing.T) {
	srv := fakeNoDCRServer(t)
	t.Cleanup(srv.Close)
	installLoopbackSeams(t)

	// Driven through a real run rather than composed by hand, so the assertion
	// is made against the redirect URI the flow actually announces.
	_, st, err := runFlow(t, mcpTargetFor(t, srv.URL+"/mcp"), "pasted-cid\npasted-csecret\n")
	require.NoError(t, err)

	guidance := oauth_mcp.ClientGuidanceForTest(st)
	require.Contains(t, guidance, st.Get(oauth_mcp.KeyRedirectURI),
		"the guidance must carry the redirect URI; that is the whole reason it exists")
	assert.Empty(t, tui.RailedNoteBudget().Overflows(guidance),
		"these lines are too wide for a note and will be wrapped or truncated")
}

// TestResultRefusesAStateThatHoldsNoToken is the fail-closed half. huh's
// accessible renderer has no error channel, so a run whose input dried up
// arrives at Result as silence rather than as a failure — and this flow stores
// an OAuth bundle, so a Result that did not check would persist a credential
// that authenticates as nobody behind a CLI reporting success.
func TestResultRefusesAStateThatHoldsNoToken(t *testing.T) {
	cases := []struct {
		name  string
		state *tui.State
	}{
		{name: "a nil State: refused rather than panicking", state: nil},
		{name: "an unanswered State: refused", state: tui.NewState()},
		{
			name: "a State whose access token is blank: refused",
			state: func() *tui.State {
				st := tui.NewState()
				st.Set(oauth_mcp.KeyAccessToken, "   ")
				return st
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stored []builtins.StoreValue
			req := builtins.Request{
				IdentityName: "demo-bot",
				Store: func(_ context.Context, v builtins.StoreValue) error {
					stored = append(stored, v)
					return nil
				},
			}
			assert.NotPanics(t, func() {
				assert.Error(t, oauth_mcp.New().Result(context.Background(), req, tc.state))
			})
			assert.Empty(t, stored, "Result stored %+v for a State holding no token", stored)
		})
	}
}
