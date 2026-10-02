//go:build integration

// End-to-end integration coverage for the OAuth-minted /mcp surface: a real
// SpiceDB (test/testspicedb), a real memory backend served over HTTP
// (pkg/memory/httpsrv), a real identityd authorization-server (DCR, PKCE
// authorize/consent/token), a real mcpfront bearer middleware + tool set, and
// a real MCP client (pkg/tools/mcp/probe) — wired together exactly as webd
// wires its own production umbrella (internal/cmd/webd's artifactViewDeps),
// minus the things an integration test has no business standing up (K8s
// apiserver, NATS, the browser).
//
// Only TestFullOAuthAndMCPFlow drives the full browser-grade OAuth dance
// (DCR -> authorize -> consent -> token). The other three mint directly
// through the same production Minter the token endpoint calls — the claim
// under test in each of them is about SpiceDB-backed authorization at tool-call
// time, not the OAuth wire protocol, which TestFullOAuthAndMCPFlow already
// covers once.
package mcpfront

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identityd"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

const (
	fixtureAccessTokenNamespace = "agentprimitives-system"
	fixtureExternalBaseURL      = "https://mcp.example.test"
	fixtureMemoryToken          = "integration-webd-token"
	fixtureSessionNamespace     = "demo-ns"
	fixtureSessionName          = "demo-session"
	fixtureClassName            = "demo-agent"
	fixtureOtherClassName       = "other-agent"
	fixtureTranscriptText       = "hello from the integration fixture"
)

// pendingFieldRe pulls the single-use pending id out of the consent form's
// hidden input (see handlers_oauthas_authorize.go's consentFormHTML).
var pendingFieldRe = regexp.MustCompile(`name="pending" value="([^"]*)"`)

// mcpFixture is the shared scaffolding every flow test below drives: a real
// SpiceDB client + schema, a real memory backend served over HTTP, a fake K8s
// client holding the AgentSession fixture(s), a production Minter, and a
// webui.Server (identityd + mcpfront) served over httptest.
type mcpFixture struct {
	owner   identity.CanonicalUserID
	spdb    *spicedb.Client
	k8s     client.Client
	mem     *memory.Local
	minter  *Minter
	baseURL string
}

// newMCPFixture stands up everything TestFullOAuthAndMCPFlow and its three
// siblings need: testspicedb + spicedb.NewClient -> schema, inmem memory +
// httpsrv on httptest (webd read-only token registered), a fake K8s client
// seeded with AgentSession demo-session (class demo-agent) in ns demo-ns plus
// a turn fixture written to memory, and a webui.Server (identityd + mcpfront)
// over httptest with an authenticate stub that always returns the fixture
// owner — the brief's "authenticate stub returns a fixed subject for
// cookie-path requests" verbatim, since none of these tests exercise a real
// browser session cookie.
func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	ctx := context.Background()

	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	spdb, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = spdb.Close() })

	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetWebdToken(fixtureMemoryToken)
	memSrv := httptest.NewServer(httpsrv.NewHandler(mem, reg))
	t.Cleanup(memSrv.Close)

	k8s := fake.NewClientBuilder().WithScheme(newMintScheme(t)).Build()

	owner := identity.CanonicalFromTrusted("mcp-fixture-owner", "integration test fixture")

	sess := sessionFixture(fixtureSessionNamespace, fixtureSessionName, fixtureClassName)
	require.NoError(t, k8s.Create(ctx, sess), "create demo-session")
	require.NoError(t, spdb.TouchSessionOwner(ctx, fixtureSessionNamespace, fixtureSessionName, owner), "touch demo-session owner")

	turnCtx := memory.WithSystemApproval(ctx, "integration test fixture")
	appender := turn.NewAppender(mem, memory.Scope{Kind: "session", ID: fixtureSessionNamespace + "/" + fixtureSessionName})
	require.NoError(t, appender.Append(turnCtx, memory.Turn{
		Index:     0,
		Role:      "user",
		Content:   []memory.ContentBlock{{Type: "text", Text: fixtureTranscriptText}},
		CreatedAt: time.Now().UTC(),
	}), "append transcript turn fixture")

	minter := &Minter{
		SpiceDB:   spdb,
		K8s:       k8s,
		Namespace: fixtureAccessTokenNamespace,
		Lifetime:  time.Hour,
	}

	deps := &integrationDeps{
		owner:      owner,
		spdb:       spdb,
		k8s:        k8s,
		mem:        mem,
		memURL:     memSrv.URL,
		minter:     minter,
		linkSigner: passthroughlink.New([]byte("integration-test-signing-key-0123456789")),
	}

	authFn := func(*http.Request) (string, bool) { return owner.Subject().String(), true }
	trustedHost := "127.0.0.1" // httptest.NewServer listens on 127.0.0.1; hostOnly strips the port on both sides.
	srv, err := webui.NewServer(
		authFn,
		nil, // beginLogin: authFn never reports "unauthenticated", so this is never consulted.
		func() string { return trustedHost },
		func() string { return "sandbox.invalid" },
		nil, // sharedOriginOK: distinct origins, never shared.
		deps,
		[]webui.WebUI{identityd.New(), ui{}},
	)
	require.NoError(t, err, "webui.NewServer")

	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	return &mcpFixture{
		owner:   owner,
		spdb:    spdb,
		k8s:     k8s,
		mem:     mem,
		minter:  minter,
		baseURL: httpSrv.URL,
	}
}

// mint mints an access token directly through the fixture's production
// Minter — the same component /oauth/token calls — for the three tests that
// aren't exercising the OAuth wire protocol itself.
func (f *mcpFixture) mint(t *testing.T, role string, scopeClasses []string, unfiltered bool) Minted {
	t.Helper()
	minted, err := f.minter.MintAccessToken(context.Background(), MintParams{
		Owner:        f.owner,
		Role:         role,
		ScopeClasses: scopeClasses,
		Unfiltered:   unfiltered,
		ClientName:   "integration test",
		ClientID:     "integration-test-client",
	})
	require.NoError(t, err, "MintAccessToken")
	return minted
}

// callTool calls name on the /mcp endpoint with bearer and requires the
// round trip itself to succeed (a non-2xx HTTP response is a test failure
// here; IsError tool results are a normal, assertable outcome handled by the
// caller).
func (f *mcpFixture) callTool(t *testing.T, bearer, name string, args any) *probe.CallOutcome {
	t.Helper()
	out, err := f.callToolRaw(context.Background(), bearer, name, args)
	require.NoError(t, err, "CallTool %s", name)
	return out
}

// callToolRaw is callTool without the NoError requirement, for the one case
// (TestRevocationIsImmediateViaTupleDelete's final call) that expects the
// round trip itself to fail with an HTTP-level error.
func (f *mcpFixture) callToolRaw(ctx context.Context, bearer, name string, args any) (*probe.CallOutcome, error) {
	return probe.CallTool(ctx, f.baseURL+"/mcp", &http.Client{}, name, args, "Authorization", "Bearer "+bearer, nil)
}

// listSessionsUntil polls list_sessions until pred holds of the decoded
// result or a short deadline passes, returning the last decoded result
// either way. coveredSessions' leg-1 (CheckAccessTokenMirror) and leg-2
// (FilterAccessTokenCoveredClasses) checks both run at SpiceDB's
// MinimizeLatency consistency — ops.go's deliberate latency/freshness
// tradeoff for cheap per-row enumeration checks (contrast
// authorizeSessionOp's single-resource checks, which are always fully
// consistent) — so a token minted microseconds earlier can briefly list as
// covering nothing. This rides out that same window the way a real polling
// client would, rather than asserting on an instant SpiceDB's own consistency
// model does not guarantee.
func listSessionsUntil(t *testing.T, f *mcpFixture, bearer string, pred func(ListSessionsOut) bool) ListSessionsOut {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last ListSessionsOut
	for {
		out := f.callTool(t, bearer, "list_sessions", nil)
		require.False(t, out.IsError, "list_sessions must succeed: %+v", out)
		require.NoError(t, json.Unmarshal([]byte(out.Content[0].Text), &last))
		if pred(last) || time.Now().After(deadline) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assertDenialText decodes out's single text block as the {"error": "..."}
// envelope toolErr produces and asserts it equals want exactly — the uniform
// no-existence-oracle text every denial in this package returns verbatim.
func assertDenialText(t *testing.T, out *probe.CallOutcome, want string) {
	t.Helper()
	require.True(t, out.IsError, "expected a tool-level denial")
	require.NotEmpty(t, out.Content, "denial result must carry a text block")
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.Content[0].Text), &body))
	assert.Equal(t, want, body.Error)
}

// integrationDeps is this file's single concrete deps value: it satisfies
// identityd.WebDeps + identityd.ConsentDeps + identityd.AccessTokenMinter
// (so identityd's full OAuth authorization-server surface mounts) AND
// mcpfront.Deps (so the /mcp surface mounts) simultaneously — the same shape
// internal/cmd/webd's artifactViewDeps uses in production, over real
// collaborators (a live SpiceDB client, a fake K8s client, a real memory
// backend) rather than webd's NATS/admind/browser-session extras this test
// has no use for.
type integrationDeps struct {
	owner      identity.CanonicalUserID
	spdb       *spicedb.Client
	k8s        client.Client
	mem        memory.Memory
	memURL     string
	minter     *Minter
	linkSigner *passthroughlink.Signer
}

// --- identityd.WebDeps -------------------------------------------------

func (d *integrationDeps) K8s() client.Client                  { return d.k8s }
func (d *integrationDeps) LinkSigner() *passthroughlink.Signer { return d.linkSigner }
func (d *integrationDeps) ExternalBaseURL() string             { return fixtureExternalBaseURL }
func (d *integrationDeps) Authenticators() map[string]channelkinds.WebAuthenticator {
	return map[string]channelkinds.WebAuthenticator{}
}
func (d *integrationDeps) IconHandler() http.Handler { return nil }
func (d *integrationDeps) InsecureTrustLinks() bool  { return false }

// --- identityd.ConsentDeps -----------------------------------------------

// ConsentClasses is a fixed list rather than a SpiceDB-derived one (contrast
// webd's production LookupStartableClasses/LookupInteractableSessions union):
// this test's authorization boundary is the three-legged CheckAccessTokenOp
// every tool call drives for real against the live SpiceDB client, not the
// checkbox list the consent screen renders. Both fixture classes are listed
// so TestScopeFilterExcludesOtherClasses' second session's class is a valid
// consent option too, even though no test currently grants it.
func (d *integrationDeps) ConsentClasses(context.Context, identity.CanonicalUserID) ([]identityd.ConsentClass, error) {
	return []identityd.ConsentClass{
		{ID: fixtureSessionNamespace + "/" + fixtureClassName, DisplayName: "Demo Agent"},
		{ID: fixtureSessionNamespace + "/" + fixtureOtherClassName, DisplayName: "Other Agent"},
	}, nil
}

// --- identityd.AccessTokenMinter ------------------------------------------

// MintAccessToken adapts identityd's mirror MintParams/Minted shapes onto the
// real mcpfront.MintParams/Minted the fixture's Minter speaks — the same
// adaptation internal/cmd/webd's artifactViewDeps.MintAccessToken performs in
// production, over the SAME *Minter the direct-mint helper (mcpFixture.mint)
// uses, so a token minted via /oauth/token and one minted directly are
// byte-for-byte the same shape.
func (d *integrationDeps) MintAccessToken(ctx context.Context, p identityd.MintParams) (identityd.Minted, error) {
	minted, err := d.minter.MintAccessToken(ctx, MintParams{
		Owner:        p.Owner,
		Role:         p.Role,
		ScopeClasses: p.ScopeClasses,
		Unfiltered:   p.Unfiltered,
		ClientName:   p.ClientName,
		ClientID:     p.ClientID,
	})
	if err != nil {
		return identityd.Minted{}, err
	}
	return identityd.Minted{Value: minted.Value, TokenID: minted.TokenID, ExpiresAt: minted.ExpiresAt}, nil
}

// --- mcpfront.Deps ---------------------------------------------------------

func (d *integrationDeps) AccessTokenAuthz() AccessTokenAuthz { return d.spdb }
func (d *integrationDeps) AccessTokenNamespace() string       { return fixtureAccessTokenNamespace }
func (d *integrationDeps) OperatorURL() string                { return d.memURL }
func (d *integrationDeps) MemoryToken() string                { return fixtureMemoryToken }
func (d *integrationDeps) Artifacts() *artifacts.Service      { return artifacts.NewService(d.mem, nil) }

func (d *integrationDeps) LookupReadableSessions(ctx context.Context, owner identity.CanonicalUserID) (spicedb.InteractableSessions, error) {
	return d.spdb.LookupInteractableSessions(ctx, owner, 200, true)
}

func (d *integrationDeps) FetchArtifact(context.Context, string, string, string) ([]byte, string, error) {
	return nil, "", fmt.Errorf("integration fixture: FetchArtifact not implemented (unused by these flows)")
}

func (d *integrationDeps) Logger() logr.Logger { return logr.Discard() }

// Compile-time proofs that integrationDeps satisfies every interface the
// fixture relies on — a signature drift on any side fails `go build`, not a
// runtime cast deep inside webui.NewServer's plugin mounting.
var (
	_ Deps                        = (*integrationDeps)(nil)
	_ identityd.WebDeps           = (*integrationDeps)(nil)
	_ identityd.ConsentDeps       = (*integrationDeps)(nil)
	_ identityd.AccessTokenMinter = (*integrationDeps)(nil)
)

// TestFullOAuthAndMCPFlow drives the complete browser-grade OAuth dance —
// protected-resource metadata, DCR, authorize, consent, token exchange — and
// then proves the minted bearer actually works against the real /mcp surface:
// exactly the six read tools are advertised, list_sessions sees demo-session,
// and get_transcript returns the fixture's own turn text (asserted on the
// TOOL RESULT, never on a reply).
func TestFullOAuthAndMCPFlow(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()
	hc := &http.Client{}

	// 1. GET /.well-known/oauth-protected-resource -> AS URL.
	presp, err := hc.Get(f.baseURL + "/.well-known/oauth-protected-resource")
	require.NoError(t, err, "GET protected-resource metadata")
	defer presp.Body.Close()
	require.Equal(t, http.StatusOK, presp.StatusCode)
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	require.NoError(t, json.NewDecoder(presp.Body).Decode(&prm))
	require.NotEmpty(t, prm.AuthorizationServers, "protected-resource metadata must name an authorization server")
	assert.Equal(t, fixtureExternalBaseURL, prm.AuthorizationServers[0])
	assert.Equal(t, fixtureExternalBaseURL+"/mcp", prm.Resource)

	// 2. DCR -> client_id.
	const redirectURI = "http://127.0.0.1:7777/cb"
	clientID, _, err := oauth.Register(ctx, hc, f.baseURL+"/oauth/register", []string{redirectURI})
	require.NoError(t, err, "DCR register")
	require.NotEmpty(t, clientID)

	// 3. GET /oauth/authorize (authenticated via the fixture's stub) -> parse
	// the pending id from the consent form HTML.
	pkce, err := oauth.NewPKCE()
	require.NoError(t, err)

	authorizeURL := f.baseURL + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {"S256"},
		"state":                 {pkce.State},
	}.Encode()
	aresp, err := hc.Get(authorizeURL)
	require.NoError(t, err, "GET /oauth/authorize")
	defer aresp.Body.Close()
	require.Equal(t, http.StatusOK, aresp.StatusCode, "authorize must render the consent form")
	abody, err := io.ReadAll(aresp.Body)
	require.NoError(t, err)
	m := pendingFieldRe.FindSubmatch(abody)
	require.Len(t, m, 2, "consent form must carry a hidden pending field")
	pendingID := string(m[1])

	// 4. POST /oauth/consent role=read scope=demo-ns/demo-agent -> code from
	// the redirect Location. CheckRedirect intercepts the 302 rather than
	// following it into an unbound loopback port.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	consentForm := url.Values{
		"pending": {pendingID},
		"role":    {accesstoken.RoleRead},
		"scope":   {fixtureSessionNamespace + "/" + fixtureClassName},
		"approve": {"1"},
	}
	creq, err := http.NewRequest(http.MethodPost, f.baseURL+"/oauth/consent", strings.NewReader(consentForm.Encode()))
	require.NoError(t, err)
	creq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	creq.Header.Set("Origin", fixtureExternalBaseURL) // CSRF origin pin: must equal ExternalBaseURL() exactly.
	cresp, err := noRedirect.Do(creq)
	require.NoError(t, err, "POST /oauth/consent")
	defer cresp.Body.Close()
	require.Equal(t, http.StatusFound, cresp.StatusCode, "approved consent must redirect with a code")
	loc, err := url.Parse(cresp.Header.Get("Location"))
	require.NoError(t, err)
	code := loc.Query().Get("code")
	require.NotEmpty(t, code, "redirect must carry an authorization code")
	assert.Equal(t, pkce.State, loc.Query().Get("state"))

	// 5. POST /oauth/token with the PKCE verifier -> bearer.
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {pkce.Verifier},
	}
	tresp, err := hc.Post(f.baseURL+"/oauth/token", "application/x-www-form-urlencoded", strings.NewReader(tokenForm.Encode()))
	require.NoError(t, err, "POST /oauth/token")
	defer tresp.Body.Close()
	require.Equal(t, http.StatusOK, tresp.StatusCode, "token exchange must succeed")
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
	}
	require.NoError(t, json.NewDecoder(tresp.Body).Decode(&tok))
	assert.True(t, strings.HasPrefix(tok.AccessToken, accesstoken.TokenPrefix))
	assert.Equal(t, "Bearer", tok.TokenType)
	assert.Equal(t, "role:read classes:"+fixtureSessionNamespace+"/"+fixtureClassName, tok.Scope)
	require.NotEmpty(t, tok.AccessToken)

	// 6/8. tools/list has exactly the six read tools (no write/send tool
	// exists in Phase 1).
	probeClient := &probe.Client{HTTP: &http.Client{}, URL: f.baseURL + "/mcp"}
	toolList, err := probeClient.ListTools(ctx, "Authorization", "Bearer "+tok.AccessToken)
	require.NoError(t, err, "tools/list")
	names := make([]string, 0, len(toolList))
	for _, tl := range toolList {
		names = append(names, tl.Name)
	}
	assert.ElementsMatch(t,
		[]string{"list_sessions", "get_session", "get_transcript", "search_memory", "list_artifacts", "get_artifact"},
		names)

	// 6. list_sessions contains demo-session.
	lso := listSessionsUntil(t, f, tok.AccessToken, func(o ListSessionsOut) bool {
		for _, s := range o.Sessions {
			if s.Namespace == fixtureSessionNamespace && s.Name == fixtureSessionName {
				return true
			}
		}
		return false
	})
	var sawDemoSession bool
	for _, s := range lso.Sessions {
		if s.Namespace == fixtureSessionNamespace && s.Name == fixtureSessionName {
			sawDemoSession = true
		}
	}
	assert.True(t, sawDemoSession, "list_sessions must contain demo-session")

	// 7. get_transcript returns the fixture text — asserted on the TOOL
	// RESULT.
	transcriptOut := f.callTool(t, tok.AccessToken, "get_transcript",
		GetTranscriptIn{Namespace: fixtureSessionNamespace, Name: fixtureSessionName})
	require.False(t, transcriptOut.IsError, "get_transcript must succeed: %+v", transcriptOut)
	var gto GetTranscriptOut
	require.NoError(t, json.Unmarshal([]byte(transcriptOut.Content[0].Text), &gto))
	var sawFixtureText bool
	for _, e := range gto.Entries {
		if e.Text == fixtureTranscriptText {
			sawFixtureText = true
		}
	}
	assert.True(t, sawFixtureText, "get_transcript must return the fixture turn text")
}

// TestRevocationIsImmediateViaTupleDelete pins the revocation race from the
// plan's Review Focus #1: deleting the SpiceDB tuples (what the AccessToken
// finalizer does) denies the NEXT call immediately at the tool layer, and
// deleting the CR too (once the bearer cache notices, at tokenCacheTTL) denies
// at the HTTP layer.
func TestRevocationIsImmediateViaTupleDelete(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()

	minted := f.mint(t, accesstoken.RoleRead, []string{fixtureSessionNamespace + "/" + fixtureClassName}, false)

	out := f.callTool(t, minted.Value, "get_transcript",
		GetTranscriptIn{Namespace: fixtureSessionNamespace, Name: fixtureSessionName})
	require.False(t, out.IsError, "freshly minted token must work: %+v", out)

	// Simulate the finalizer: tuples gone, CR still present.
	require.NoError(t, f.spdb.DeleteAccessTokenTuples(ctx, minted.TokenID))

	out2 := f.callTool(t, minted.Value, "get_transcript",
		GetTranscriptIn{Namespace: fixtureSessionNamespace, Name: fixtureSessionName})
	assertDenialText(t, out2, "get_transcript: "+errSessionNotAccessible.Error())

	// Now delete the CR itself. The bearer middleware's tokenCache still
	// authenticates from its in-process snapshot until that snapshot goes
	// stale (tokenCacheTTL) and a relist notices the CR is gone — Routes()
	// wires the middleware to the real clock (time.Now), so this is a real
	// wait, not a simulated one.
	require.NoError(t, f.k8s.Delete(ctx, &spiceboxv1alpha1.AccessToken{
		ObjectMeta: metav1.ObjectMeta{Name: minted.TokenID, Namespace: fixtureAccessTokenNamespace},
	}))
	time.Sleep(tokenCacheTTL + 2*time.Second)

	_, err := f.callToolRaw(ctx, minted.Value, "get_transcript",
		GetTranscriptIn{Namespace: fixtureSessionNamespace, Name: fixtureSessionName})
	require.Error(t, err, "a token whose CR is gone must fail at the HTTP layer, not the tool layer")
	var herr *probe.HTTPError
	require.ErrorAs(t, err, &herr, "must be an HTTP-level failure")
	assert.Equal(t, http.StatusUnauthorized, herr.StatusCode)
}

// TestOwnerDeniedKillsToken pins the plan's Review Focus #4: a valid,
// full-role, unfiltered token whose OWNER has since been denied on the
// session must be refused — leg 3 (OwnerHas) of the three-legged check, not
// leg 1 or leg 2. Exercised TWICE to pin durability (the bronzethread rule):
// a missing decision-writer could pass the first call and leak state the
// second.
func TestOwnerDeniedKillsToken(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()

	require.NoError(t, f.spdb.TouchDeniedUser(ctx, fixtureSessionNamespace, fixtureSessionName, f.owner))

	minted := f.mint(t, accesstoken.RoleFull, nil, true)

	for i := 1; i <= 2; i++ {
		out := f.callTool(t, minted.Value, "get_transcript",
			GetTranscriptIn{Namespace: fixtureSessionNamespace, Name: fixtureSessionName})
		assertDenialText(t, out, "get_transcript: "+errSessionNotAccessible.Error())
		t.Logf("call %d: denied as expected", i)
	}
}

// TestScopeFilterExcludesOtherClasses pins the plan's Review Focus #3: a
// token scoped to one agentclass must not see — via list_sessions — or reach
// — via get_transcript — a session of a different class, even though the same
// owner may read it directly. The denial for the excluded session must read
// identically to the denial for a session that doesn't exist at all: no
// existence oracle.
func TestScopeFilterExcludesOtherClasses(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()

	const otherSessionName = "other-session"
	other := sessionFixture(fixtureSessionNamespace, otherSessionName, fixtureOtherClassName)
	require.NoError(t, f.k8s.Create(ctx, other), "create other-session")
	require.NoError(t, f.spdb.TouchSessionOwner(ctx, fixtureSessionNamespace, otherSessionName, f.owner))

	minted := f.mint(t, accesstoken.RoleRead, []string{fixtureSessionNamespace + "/" + fixtureClassName}, false)

	lso := listSessionsUntil(t, f, minted.Value, func(o ListSessionsOut) bool {
		return len(o.Sessions) == 1 && o.Sessions[0].Name == fixtureSessionName
	})
	names := make([]string, 0, len(lso.Sessions))
	for _, s := range lso.Sessions {
		names = append(names, s.Name)
	}
	assert.Equal(t, []string{fixtureSessionName}, names,
		"list_sessions must return only the session whose class the token's scope covers")

	otherOut := f.callTool(t, minted.Value, "get_transcript",
		GetTranscriptIn{Namespace: fixtureSessionNamespace, Name: otherSessionName})
	require.True(t, otherOut.IsError, "out-of-scope session must be denied: %+v", otherOut)

	missingOut := f.callTool(t, minted.Value, "get_transcript",
		GetTranscriptIn{Namespace: fixtureSessionNamespace, Name: "does-not-exist"})
	require.True(t, missingOut.IsError, "nonexistent session must be denied: %+v", missingOut)

	var otherBody, missingBody struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(otherOut.Content[0].Text), &otherBody))
	require.NoError(t, json.Unmarshal([]byte(missingOut.Content[0].Text), &missingBody))
	assert.Equal(t, missingBody.Error, otherBody.Error,
		"a scope-excluded session must read identically to a nonexistent one — no existence oracle")
}
