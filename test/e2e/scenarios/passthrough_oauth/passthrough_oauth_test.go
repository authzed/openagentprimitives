//go:build e2e

// Package passthrough_oauth_test is the Slice-3 phase-ε end-to-end
// scenario for the user-passthrough OAuth flow. It drives every moving
// part of the
//
//	"operator parks → channelsd publishes credential_request with
//	 OAuth button → identityd /link/oauth/<credname> → fakeoauth
//	 /authorize (DCR + PKCE) → /oauth/callback/<credname> → PutOAuthToken
//	 → operator un-parks"
//
// chain in a single in-process process.
//
// What this proves end-to-end:
//
//  1. The AgentSession reconciler parks an identityMode=userPassthrough
//     session in AwaitingCredentials and writes a SessionUserIdentity
//     listing linear-oauth in MissingCredentials. Same gate as Slice 2;
//     the MCPServer's spec.auth.type=oauth differentiates the rendered
//     button (one OAuth button per OAuth credential, no shared signed
//     deep-link).
//
//  2. The CredentialRequestWatcher (channelsd's E1 work, instantiated
//     directly in this test because the e2e harness's channelsd
//     equivalent doesn't run it) lists the parked session and NATS-publishes
//     a credential_link interaction_request envelope whose Actions contain
//     ONE entry whose URL is the unified <identityd>/link?d=&sig= menu; per
//     credential OAuth routing happens via that menu's per-row action. The
//     watcher's ε8 partition logic looks up the MCPServer, sees
//     Spec.Auth.Type=oauth, and routes the credential to the OAuth branch —
//     covered by unit tests; the E2E asserts the resulting envelope shape
//     against the fake driver's recorded interaction-prompt queue.
//
//  3. identityd's /link/oauth/<credname> handler (ε5) cookie-gates the
//     request, discovers OAuth metadata against the MCPServer's URL,
//     runs DCR if available, generates PKCE, stashes the state entry,
//     and 302s the browser at the provider's /authorize endpoint. The
//     E2E uses the fakeoauth provider (ε9) as the upstream; same package-
//     level newOAuthHTTPClient seam ε5/ε6's unit tests use — exposed
//     here via the e2e-tagged SetOAuthHTTPClientForTesting export so the
//     scenario can install the httptest-server's client without reaching
//     into identityd's package internals.
//
//  4. fakeoauth's /authorize auto-grants without a consent UI, validates
//     the required PKCE params, and 302s back to identityd's
//     /oauth/callback/<credname> with ?code=&state=.
//
//  5. identityd's /oauth/callback/<credname> handler (ε6) re-verifies
//     the state token (single-use), gates by cookie subject, exchanges
//     the code with the fakeoauth /token endpoint (which verifies the
//     PKCE code_verifier against the stored code_challenge), and
//     persists via useridentity.PutOAuthToken — creating the master
//     Secret with the OAuth multi-key shape (access_token +
//     refresh_token + expires_at + token_type + scope) and the
//     UserIdentity entry with Type=oauth + OAuth.SecretRef.
//
//  6. The operator's UserIdentity watch re-enqueues the parked session;
//     the passthrough gate un-parks it (CredentialsReady=True,
//     SessionUserIdentity Ready=True).
//
// The cookie-mint shortcut mirrors Slice 2's passthrough_selfservice
// approach: the OIDC bootstrap path is exercised by D3's unit tests; the
// E2E focuses on the OAuth-specific composition that crosses the
// channelsd / identityd / fakeoauth boundaries.
//
// Critical wiring decisions:
//
//   - safehttp.Client (the production OAuth HTTP client) refuses
//     loopback destinations — incompatible with httptest. The test
//     installs identityd.SetOAuthHTTPClientForTesting (e2e-only export)
//     to drop in a permissive *http.Client.
//
//   - DCR registers <base>/oauth/callback while authorize-time uses
//     <base>/oauth/callback/<credname>. ε5/ε6's report flagged this
//     as a potential mismatch with RFC 7591-strict providers. fakeoauth
//     does NOT byte-match redirect_uri against the registered list (its
//     /authorize accepts whatever's passed), so the mismatch doesn't
//     break this scenario — the production fix lands in a follow-up
//     slice when MCPServer gains per-server OAuth-client config.
//
// No real names: alice / Triage Bot / Linear / example.com are all
// fictional per AGENTS.md.
package passthrough_oauth_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/platform/identityd"
	"github.com/authzed/openagentprimitives/test/e2e"
	"github.com/authzed/openagentprimitives/test/e2e/internal/fakeoauth"
)

const (
	// starterEmail is the human starter the test impersonates. Mapped
	// to a canonical SpiceDB subject via identity.Principal.Canonical() at the
	// pipeline boundary; the operator's passthrough gate reads the
	// canonical from sess.Annotations[AnnotationStartedByCanonicalID].
	starterEmail = "alice@example.com"

	// linearCred is the credential the AgentClass's referenced MCPServer
	// (linear-mcp in manifests.yaml) declares via spec.auth.credential.
	// Spec.Auth.Type="oauth" drives the OAuth-typed credential branch in
	// the credential_request watcher.
	linearCred = "linear-oauth"

	// signingKey is the 32-byte HMAC key SHARED by the in-test
	// CredentialRequestWatcher and the in-test identityd. Both ends of
	// the link-mint / link-verify path use it; the cookie-mint shortcut
	// in the web flow also uses it.
	signingKey = "passthrough-oauth-e2e-test-key32" // 32 bytes
)

// TestPassthroughOAuth is the end-to-end assertion for Slice 3 ε. One
// linear test body so a failure surfaces the step that broke; each
// section logs the assertion intent before driving it.
func TestPassthroughOAuth(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	// 1. Start the fakeoauth provider FIRST so its URL is known before
	//    the MCPServer manifest is applied. The provider's URL goes into
	//    spec.server.url (replacing the {{OAUTH_URL}} sentinel) so OAuth
	//    discovery + DCR + authorize + token-exchange all land on the
	//    in-process fake.
	op := fakeoauth.NewServer()
	t.Cleanup(op.Close)

	// Split the MCPServer doc out so it can be applied AFTER the
	// fakeoauth URL is known. The same pattern Slice 2's
	// passthrough_selfservice uses for {{MCP_URL}} — the harness's
	// e2e.Start applies the rest of the manifests, then the test applies
	// the MCPServer with the rewritten URL.
	rawManifests := string(manifests)
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(rawManifests)

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    starterEmail,
	})

	// Apply the MCPServer now that op.URL() is known.
	h.ApplyManifest(strings.ReplaceAll(mcpServerDoc, "{{OAUTH_URL}}", op.URL()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// envtest doesn't auto-create namespaces; useridentity.PutOAuthToken
	// (called by /oauth/callback) needs agentprimitives-identities to
	// exist for its master-Secret Create.
	createIdentitiesNamespace(t, ctx, h.K8s)

	// 2. Install the OAuth HTTP-client seam. identityd's production code
	//    path uses safehttp.Client, which (correctly) refuses 127.0.0.1
	//    destinations — incompatible with httptest. The e2e-tagged
	//    SetOAuthHTTPClientForTesting export drops in fakeoauth's
	//    permissive client.
	restore := identityd.SetOAuthHTTPClientForTesting(op.Client())
	t.Cleanup(restore)

	// The shared HMAC signing key — channelsd's watcher mints links with
	// it; identityd verifies links + cookies with it; the test mints the
	// cookie with it. Same key, all three roles.
	// iss/aud default: matches production channelsd's link-minting role
	// (iss=channelsd, aud=identityd). The cookie Mint call below
	// explicitly overrides Issuer=identityd so checkOIDCCookie's
	// WithExpectedIssuer(IssuerIdentityd) gate passes.
	signer := passthroughlink.New([]byte(signingKey),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	// 3. Start in-process identityd on a httptest.Server. The
	//    ExternalBaseURL chicken-and-egg is solved by binding the test
	//    server first (unstarted), then constructing the server with the
	//    known URL, then starting. See startIdentityd.
	idBaseURL := startIdentityd(t, h.K8s, signer)
	t.Logf("identityd: %s, fakeoauth: %s", idBaseURL, op.URL())

	// 4. Construct + run the CredentialRequestWatcher directly. The e2e
	//    harness's channelsd-equivalent plumbing doesn't wire one (it
	//    predates Slice 2), so the test instantiates the watcher with the
	//    signer/baseURL it controls and runs it as a goroutine bound to
	//    its own context. Task 6 flipped delivery to a NATS publish — dial a
	//    connection against the harness's embedded NATS server so the publish
	//    reaches the harness's already-running outbound relay (see
	//    dialHarnessNATS).
	natsConn := dialHarnessNATS(t, h.NATSURL, "e2e-pt-oauth-crw")
	startCredentialRequestWatcher(t, h.K8s, signer, idBaseURL, natsConn.Publish)

	// Provide a minimal LLM script: once the session un-parks, the
	// operator transitions it past AwaitingCredentials and spawns a
	// runner.Loop. The script is intentionally vacuous — the test
	// asserts only on cluster state + the OAuth flow side-effects.
	h.LLM.OnUserMessage("connect my linear").Reply(e2e.RespondToUser("ok"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	// Block until the AgentClass becomes Valid=True (finalSuccess in the
	// MCPServer controller stamps it unconditionally even when the probe
	// of fakeoauth's URL doesn't speak MCP).
	h.WaitForAgentClassValid("triage-bot", 30*time.Second)

	// Step 5: drive the inbound that spawns the parked session.
	h.SendUserMessage("connect my linear")

	// Step 6: wait for the operator to park the session.
	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err)
	starterCanonical := starterCanon.Subject()
	t.Logf("starterCanonical=%s", starterCanonical)
	sess := waitForParkedSession(t, ctx, h.K8s, starterCanonical)

	// Step 6a: verify SUI lists linear-oauth in MissingCredentials.
	var sui spiceboxv1alpha1.SessionUserIdentity
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKeyFromObject(sess), &sui),
		"get SessionUserIdentity %s/%s", sess.Namespace, sess.Name)
	assert.Equal(t, []string{linearCred}, sui.Status.MissingCredentials,
		"MissingCredentials must list linear-oauth")

	// Step 7: wait for the credential_link interaction_request with an OAuth
	// link action pointing at the unified /link menu. The watcher polls
	// every 5s; the first reconcile fires at Run() start, then again
	// every tick. Task 6 flipped delivery from a direct credential_request
	// sub-channel send to a NATS-published interaction_request —
	// Driver.InteractionPrompts() is where it lands now.
	drv := fake.DriverFor(sess.Namespace, sess.Spec.InputChannel.Name)
	require.NotNil(t, drv, "fake.DriverFor(%s/%s) — listener may not have started",
		sess.Namespace, sess.Spec.InputChannel.Name)
	rec := waitForCredentialPrompt(t, ctx, drv, sess.Namespace+"/"+sess.Name)

	// Post-unified-link refactor: channelsd no longer emits a per-
	// credential OAuth URL on the envelope. It emits ONE generic
	// "Connect …" action pointing at the unified /link?d=&sig= menu
	// page; per-credential OAuth routing happens via the menu's
	// per-row action (GET /link/oauth/<credname>, cookie-gated).
	// Assert the envelope carries the unified action, then construct
	// the per-credential OAuth URL directly to drive the rest of the
	// flow (cookie-gated end-to-end).
	var menuLinkURL string
	wantMenuPrefix := idBaseURL + "/link?d="
	for _, a := range rec.Payload.Actions {
		if strings.HasPrefix(a.URL, wantMenuPrefix) {
			menuLinkURL = a.URL
			break
		}
	}
	require.NotEmpty(t, menuLinkURL,
		"credential_link interaction_request should carry an action with URL prefix %q; got actions=%+v",
		wantMenuPrefix, rec.Payload.Actions)
	oauthLinkURL := idBaseURL + "/link/oauth/" + linearCred

	// Step 8: drive the web flow.
	//
	// 8a. Mint a cookie with the same signer identityd's OIDC callback
	// would have set. Subject must equal the link's Subject for the
	// /link/oauth cookie-gate AND the /oauth/callback cookie-gate to
	// both pass.
	// checkOIDCCookie verifies iss=identityd aud=identityd after the
	// JWT-claims hardening; override the Signer's default iss=channelsd
	// here so the cookie matches what production setTrustLinkCookie writes.
	cookieRaw, err := signer.Mint(passthroughlink.Payload{
		Issuer:    passthroughlink.IssuerIdentityd,
		Audience:  passthroughlink.AudienceIdentityd,
		Subject:   starterCanonical,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err, "mint idd_session cookie")

	// HTTP client with a cookie jar; auto-follows redirects so the chain
	//   /link/oauth/<cred> → fakeoauth /authorize → /oauth/callback/<cred>
	//   → /my/accounts?linked=<cred>
	// is traversed in a single httpClient.Do call. The jar carries the
	// idd_session cookie across all hops (including the fakeoauth hop —
	// fakeoauth doesn't read the cookie, but the jar's per-host scope
	// means the cookie won't leak there anyway).
	jar, err := cookiejar.New(nil)
	require.NoError(t, err, "new cookie jar")

	idURL, err := url.Parse(idBaseURL)
	require.NoError(t, err, "parse idBaseURL")
	jar.SetCookies(idURL, []*http.Cookie{{
		Name:  "idd_session",
		Value: cookieRaw,
		Path:  "/",
	}})

	httpClient := &http.Client{Jar: jar}

	// 8b. GET the OAuth link URL. The default redirect-follow chases
	// the entire chain; the final response should be the portal page
	// (200) at /my/accounts?linked=linear-oauth.
	req, err := http.NewRequest(http.MethodGet, oauthLinkURL, nil)
	require.NoError(t, err, "build GET %s request", oauthLinkURL)
	resp, err := httpClient.Do(req)
	require.NoError(t, err, "GET %s (followed)", oauthLinkURL)
	finalBody := readAndCloseBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"final response after redirect chain should be 200; got %d body=%s", resp.StatusCode, finalBody)
	// Final URL should be /my/accounts?linked=linear-oauth (the
	// callback's terminal redirect target).
	assert.Equal(t, idBaseURL+"/my/accounts", resp.Request.URL.Scheme+"://"+resp.Request.URL.Host+resp.Request.URL.Path,
		"final landing page should be /my/accounts")
	assert.Equal(t, linearCred, resp.Request.URL.Query().Get("linked"),
		"final URL should carry ?linked=%s", linearCred)

	// Step 9: assert the post-state of the cluster.
	uiName := useridentity.NameForSubject(starterCanonical)
	secName := useridentity.MasterSecretName(uiName, linearCred)

	// 9a. Master Secret has the OAuth multi-key shape — distinct from
	// the PAT shape (which writes only data[token]).
	var sec corev1.Secret
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      secName,
	}, &sec), "get master Secret for %s/%s", uiName, linearCred)
	assert.NotEmpty(t, sec.Data["access_token"], "OAuth access_token persisted")
	assert.NotEmpty(t, sec.Data["refresh_token"], "OAuth refresh_token persisted")
	assert.NotEmpty(t, sec.Data["token_type"], "OAuth token_type persisted")
	assert.Equal(t, "Bearer", string(sec.Data["token_type"]),
		"fakeoauth default token_type=Bearer")
	assert.NotEmpty(t, sec.Data["expires_at"], "OAuth expires_at persisted (RFC 3339)")

	// expires_at format check: PutOAuthToken writes RFC 3339, refresh
	// controller parses RFC 3339; the round-trip is what production
	// depends on.
	_, parseErr := time.Parse(time.RFC3339, string(sec.Data["expires_at"]))
	assert.NoError(t, parseErr,
		"expires_at must be RFC 3339-parseable: got %q", sec.Data["expires_at"])

	// 9b. UserIdentity has the credential with Type=oauth and an
	// OAuth.SecretRef pointing at the master Secret. Distinct from the
	// PAT shape (which sets Static.SecretRef + Type empty/static).
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Name: uiName}, &ui),
		"get UserIdentity %s", uiName)
	assert.Equal(t, starterCanonical.String(), ui.Spec.Subject)
	require.Len(t, ui.Spec.Credentials, 1,
		"UserIdentity should have exactly one credential after a single OAuth dance")

	cred := ui.Spec.Credentials[0]
	assert.Equal(t, linearCred, cred.Name)
	assert.Equal(t, "oauth", cred.Type, "credential Type must be 'oauth' (not 'static')")
	require.NotNil(t, cred.OAuth,
		"OAuth credential source must be set (not Static — OAuth + Static are mutually exclusive)")
	assert.Equal(t, secName, cred.OAuth.SecretRef.Name,
		"OAuth.SecretRef points at the master Secret")
	assert.Nil(t, cred.Static,
		"Static must remain nil for OAuth credentials")

	// 9c. Confirm fakeoauth observed the full dance — discovery, DCR,
	// authorize, token — so a future regression (e.g. callback skipping
	// the token exchange) surfaces here, not just at the Secret check.
	paths := map[string]int{}
	for _, r := range op.RecordedRequests() {
		paths[r.Path]++
	}
	assert.GreaterOrEqual(t, paths["/.well-known/oauth-authorization-server"], 1,
		"fakeoauth /.well-known must have been hit at least once (discovery)")
	assert.GreaterOrEqual(t, paths["/register"], 1,
		"fakeoauth /register must have been hit at least once (DCR)")
	assert.Equal(t, 1, paths["/authorize"],
		"fakeoauth /authorize must have been hit exactly once (no consent UI loop)")
	assert.Equal(t, 1, paths["/token"],
		"fakeoauth /token must have been hit exactly once (single-code exchange)")

	// Step 10: poll until the operator un-parks. The operator watches
	// UserIdentity and re-enqueues the parked session; the passthrough
	// gate then sees the credential present, writes SUI Ready=True, and
	// sets CredentialsReady=True on the session.
	waitForUnpark(t, ctx, h.K8s, sess.Namespace, sess.Name)
}

// ----- helpers -----------------------------------------------------------

// startIdentityd brings up an in-process identityd HTTP server on a
// httptest.Server. Returns the externally reachable base URL.
//
// Mirrors the chicken-and-egg pattern from Slice 2/2.5: bind an
// unstarted httptest.Server first (its URL is then computable),
// construct identityd with that URL, point the test server's Handler at
// the real handler.
func startIdentityd(t *testing.T, c client.Client, signer *passthroughlink.Signer) string {
	t.Helper()

	var srv *identityd.Server
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handler().ServeHTTP(w, r)
	}))
	ts.Start()
	t.Cleanup(ts.Close)

	fakeAuth := (fake.Kind{}).WebAuthenticator(channelkinds.WebAuthDeps{ExternalBaseURL: ts.URL})
	srv = identityd.NewServer(identityd.Deps{
		K8s:             c,
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return ts.URL },
		Authenticators: map[string]channelkinds.WebAuthenticator{
			"fake": fakeAuth,
		},
	})

	return ts.URL
}

// startCredentialRequestWatcher constructs a pipeline.CredentialRequestWatcher
// pointed at the test's signer + identityd base URL, then runs it in a
// goroutine bound to its own context.
//
// Owning the context here keeps cleanup self-contained: t.Cleanup
// cancels AND drains the goroutine in one shot, regardless of LIFO
// ordering relative to other cleanups.
func startCredentialRequestWatcher(
	t *testing.T,
	c client.Client,
	signer *passthroughlink.Signer,
	idBaseURL string,
	natsPublish channelevents.PublishFunc,
) {
	t.Helper()
	w := &pipeline.CredentialRequestWatcher{
		K8s:             c,
		Senders:         &testSubChannelResolver{c: c},
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return idBaseURL },
		NATSPublish:     natsPublish,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Logf("credential request watcher did not exit within 2s")
		}
	})
}

// dialHarnessNATS opens a fresh *nats.Conn against the e2e harness's own
// embedded NATS server (h.NATSURL). Task 6 flipped CredentialRequestWatcher
// from a direct SubChannelSenderFor send to a NATS publish of
// interaction_request(credential_link); this connection is what lets that
// publish reach the harness's already-running outbound relay (subscribed to
// ap.session.*.*.out.> on its own unexported connection — a separate client
// connection to the same embedded server still gets routed). Mirrors the
// dial pattern in test/e2e/scenarios/passthrough_revoke_propagation.
func dialHarnessNATS(t *testing.T, natsURL, name string) *nats.Conn {
	t.Helper()
	conn, err := nats.Connect(natsURL,
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1), nats.Name(name))
	require.NoError(t, err, "dial harness NATS as %q", name)
	t.Cleanup(func() {
		if err := conn.Drain(); err != nil {
			t.Logf("%s: nats drain on cleanup: %v", name, err)
		}
	})
	return conn
}

// testSubChannelResolver satisfies pipeline.SubChannelSenderResolver
// against the channel registry. Mirrors the helper in Slice 2's
// passthrough_selfservice scenario; inlined here so the test doesn't
// reach into harness internals.
type testSubChannelResolver struct {
	c client.Client
}

func (r *testSubChannelResolver) SubChannelSenderFor(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	name string,
) (channelkinds.Sender, error) {
	ch, sec, k, err := resolve.ForSession(ctx, r.c, sess)
	if err != nil {
		return nil, err
	}
	deps := channelkinds.Deps{
		Channel:     ch,
		Secret:      sec,
		K8sClient:   r.c,
		NATSPublish: func(string, []byte) error { return nil },
	}
	return k.SubChannelSender(name, deps), nil
}

// createIdentitiesNamespace creates the agentprimitives-identities
// namespace; envtest doesn't auto-create namespaces, and PutOAuthToken's
// master-Secret Create lands there.
func createIdentitiesNamespace(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace},
	}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %s namespace: %v", spiceboxv1alpha1.IdentitiesNamespace, err)
	}
}

// waitForParkedSession polls until an AgentSession in default namespace
// reaches AwaitingCredentials AND carries the expected starter
// annotation. Returns the parked session.
func waitForParkedSession(t *testing.T, ctx context.Context, c client.Client, starterCanonical identity.Subject) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last spiceboxv1alpha1.AgentSession
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := c.List(ctx, &list, client.InNamespace("default")); err != nil {
			t.Logf("waitForParkedSession: list: %v", err)
			time.Sleep(150 * time.Millisecond)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			last = *s
			if s.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] != starterCanonical.String() {
				continue
			}
			if s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials {
				return s
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("waitForParkedSession: no session reached AwaitingCredentials within 30s; last observed: %s/%s phase=%q starter=%q",
		last.Namespace, last.Name, last.Status.Phase,
		last.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID])
	return nil
}

// waitForCredentialPrompt polls the fake Driver's recorded interaction-prompt
// queue until a credential_link interaction_request for sessionRef appears.
// Post-Task-6-flip counterpart of the old credential_request-sub-channel
// based waiter — see startCredentialRequestWatcher's doc comment.
func waitForCredentialPrompt(t *testing.T, ctx context.Context, drv *fake.Driver, sessionRef string) fake.InteractionPrompt {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range drv.InteractionPrompts() {
			if r.Payload.Category != categories.CredentialLink {
				continue
			}
			if r.Payload.AgentSessionRef.Namespace+"/"+r.Payload.AgentSessionRef.Name == sessionRef {
				return r
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waitForCredentialPrompt: context cancelled before envelope for %s arrived", sessionRef)
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("waitForCredentialPrompt: no credential_link interaction_request for %s within 30s (recorded %d total)",
		sessionRef, len(drv.InteractionPrompts()))
	return fake.InteractionPrompt{}
}

// waitForUnpark polls until both the session's CredentialsReady=True
// and the SessionUserIdentity Ready=True. The operator clears
// MissingCredentials AT THE SAME TIME it sets Ready=True (single
// Patch), so a single condition check covers both.
func waitForUnpark(t *testing.T, ctx context.Context, c client.Client, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var sess spiceboxv1alpha1.AgentSession
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err == nil {
			credReady := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
			if credReady != nil && credReady.Status == metav1.ConditionTrue &&
				sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials {
				var sui spiceboxv1alpha1.SessionUserIdentity
				if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sui); err == nil {
					suiReady := meta.FindStatusCondition(sui.Status.Conditions, spiceboxv1alpha1.SessionUserIdentityConditionReady)
					if suiReady != nil && suiReady.Status == metav1.ConditionTrue {
						return
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waitForUnpark: context cancelled before session %s/%s un-parked", ns, name)
		case <-time.After(250 * time.Millisecond):
		}
	}

	// Diagnostic dump on timeout — name the missing signal so the test
	// output is debuggable without a re-run.
	var sess spiceboxv1alpha1.AgentSession
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		t.Fatalf("waitForUnpark: session %s/%s never un-parked AND final Get failed: %v", ns, name, err)
	}
	credReady := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
	var sui spiceboxv1alpha1.SessionUserIdentity
	_ = c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sui)
	suiReady := meta.FindStatusCondition(sui.Status.Conditions, spiceboxv1alpha1.SessionUserIdentityConditionReady)
	t.Fatalf("waitForUnpark: session %s/%s did not un-park within 60s\n  session.Phase=%q\n  CredentialsReady=%+v\n  SUID.MissingCredentials=%v\n  SUID.Ready=%+v",
		ns, name, sess.Status.Phase, credReady, sui.Status.MissingCredentials, suiReady)
}

// splitMCPServerFromManifests separates the MCPServer YAML document
// from the rest of the multi-doc YAML. Mirrors the helper in Slice 2's
// passthrough_selfservice scenario — the MCPServer's spec.server.url
// contains the {{OAUTH_URL}} sentinel substituted with op.URL() after
// the fakeoauth provider has started.
func splitMCPServerFromManifests(yamlBlob string) (mcpServerDoc, remaining string) {
	docs := strings.Split(yamlBlob, "\n---")
	var mcp, rest []string
	for _, doc := range docs {
		if strings.Contains(doc, "kind: MCPServer") {
			mcp = append(mcp, doc)
		} else {
			rest = append(rest, doc)
		}
	}
	return strings.Join(mcp, "\n---"), strings.Join(rest, "\n---")
}

// readAndCloseBody reads + closes the response body.
func readAndCloseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return string(b)
}
