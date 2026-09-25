//go:build e2e

// Package passthrough_selfservice_test is the Slice-2 user-passthrough
// end-to-end scenario. It drives every moving part of the
// "operator parks → channelsd publishes credential_request →
// identityd /link form → /link/submit persists → operator un-parks"
// flow in a single in-process process.
//
// What this proves end-to-end:
//
//  1. The AgentSession reconciler parks an identityMode=userPassthrough
//     session in AwaitingCredentials, writes the SessionUserIdentity with
//     MissingCredentials, and stamps the {what, why} explanation derived
//     from MCPServer.spec.auth.provider.
//
//  2. The CredentialRequestWatcher (channelsd's E1 work, instantiated
//     directly in this test because the e2e harness's channelsd
//     equivalent doesn't run it) lists the parked session, mints a
//     passthroughlink-signed deep-link, and dispatches a
//     KindCredentialRequest envelope on the fake channel's
//     credential_request sub-channel. The fake sub-channel sender (E3)
//     records the envelope; the test asserts on SessionRef +
//     RecipientCanonical + LinkURL + What.
//
//  3. identityd's /link handler (D2) renders the form when the visitor
//     has a valid idd_session cookie matching the link's Subject. The
//     test mints the cookie with the same passthroughlink.Signer that
//     channelsd's watcher used, so the OIDC redirect path is bypassed
//     entirely — that path is covered by D3's unit tests; this scenario
//     focuses on the parts that ONLY work together.
//
//  4. identityd's /link/submit handler (D2) re-verifies the gates and
//     calls useridentity.PutToken, which creates the master Secret in
//     IdentitiesNamespace + the cluster-scoped UserIdentity.
//
//  5. The operator's UserIdentity watch re-enqueues the parked session
//     and the passthrough gate un-parks it (CredentialsReady=True,
//     SessionUserIdentity Ready=True).
//
// The cookie-mint shortcut is the right test pattern because the OIDC
// flow itself is exercised in D3's unit tests; the E2E test should test
// only what cross-component composition can break.
//
// No real names: alice / Triage Bot / Linear / example.invalid are all
// fictional per AGENTS.md.
package passthrough_selfservice_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
	rbacv1 "k8s.io/api/rbac/v1"
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
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// starterEmail is the human starter the test impersonates. Mapped
	// to a canonical SpiceDB subject via identity.Principal.Canonical() at the
	// pipeline boundary; the operator's passthrough gate reads the
	// canonical from sess.Annotations[AnnotationStartedByCanonicalID].
	starterEmail = "alice@example.com"

	// linearCred is the credential the AgentClass's referenced
	// MCPServer (linear-mcp in manifests.yaml) declares via
	// spec.auth.credential. The starter must link it for the session
	// to un-park.
	linearCred = "linear-oauth"

	// linearProvider is the user-visible service label the operator's
	// explanation builder resolves from MCPServer.spec.auth.title into
	// SessionUserIdentity.Status.Explanation.Items[].Title.
	linearProvider = "Linear"

	// patValue is the bearer-token value the test posts to /link/submit.
	// Hex chosen to avoid accidental matches with other env vars or
	// fixture strings; identityd persists it under data["token"] on the
	// master Secret + as the cred's static.secretRef target.
	patValue = "pat-e2e-passthrough-token-c1a2b3d4"

	// signingKey is the 32-byte HMAC key SHARED by the in-test
	// CredentialRequestWatcher and the in-test identityd. Both ends of
	// the link-mint / link-verify path use it; the cookie-mint
	// shortcut in the web flow also uses it. Production wiring keeps
	// the same key in the spicebox-passthrough-link-key Secret —
	// the test owns both ends so no Secret read is needed.
	signingKey = "passthrough-selfservice-test-key" // 32 bytes
)

// TestPassthroughSelfService is the end-to-end assertion for Slice 2.
// One linear test body so a failure surfaces the step that broke (each
// section logs the assertion intent before driving it).
func TestPassthroughSelfService(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	// Split the MCPServer doc out so it can be applied AFTER the harness's
	// MCPStub URL is known. The MCPServer's spec.server.url must resolve
	// from the in-process MCPStub or the InProcessRunnerFactory's MCP probe
	// will fail (probing https://*.invalid hangs the DNS resolver for the
	// probe-timeout window every reconcile, and the runner never spawns,
	// which means the controller never reaches the Status().Update that
	// flushes CredentialsReady=True).
	rawManifests := string(manifests)
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(rawManifests)

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    starterEmail,
	})

	// Apply the MCPServer now that h.MCP.URL() is known. The MCPStub has
	// no registered tools — the scenario does not exercise tool dispatch,
	// only the parking / un-parking gate and the channelsd watcher's
	// envelope publication.
	h.ApplyManifest(strings.ReplaceAll(mcpServerDoc, "{{MCP_URL}}", h.MCP.URL()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// The harness's envtest doesn't auto-create the identities namespace.
	// useridentity.PutToken (called by /link/submit) will fail with
	// "not found" on its master Secret Create otherwise.
	createIdentitiesNamespace(t, ctx, h.K8s)

	// The shared HMAC signing key — channelsd's watcher mints links
	// with it; identityd verifies links + cookies with it; the test
	// mints the cookie with it. Same key, all three roles.
	//
	// iss/aud default: channelsd's watcher Mint auto-populates iss=
	// channelsd aud=identityd (matches production). The cookie Mint
	// below explicitly overrides Issuer to identityd, matching the
	// production setTrustLinkCookie path.
	signer := passthroughlink.New([]byte(signingKey),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	// Start in-process identityd on a httptest.Server. The
	// ExternalBaseURL chicken-and-egg is solved by binding the test
	// server first (unstarted), then constructing the server with the
	// known URL, then starting. See startIdentityd.
	idBaseURL := startIdentityd(t, h.K8s, signer)
	t.Logf("identityd: %s", idBaseURL)

	// Construct + run the CredentialRequestWatcher directly. The e2e
	// harness's channelsd-equivalent plumbing doesn't wire one (it
	// predates Slice 2), so the test instantiates the watcher with the
	// signer/baseURL it controls and runs it as a goroutine bound to
	// its own context (cleaned up via t.Cleanup).
	//
	// Task 6 flipped the watcher from a direct SubChannelSenderFor
	// send to a NATSPublish of interaction_request(credential_link) —
	// dial a fresh connection against the harness's own embedded NATS
	// server (h.NATSURL) so the publish reaches the harness's
	// already-running outbound relay (subscribed to
	// ap.session.*.*.out.>), which resolves the fake kind's
	// "interaction" sub-channel sender exactly as production's relay
	// would for any channelsd-hosted kind.
	natsConn := dialHarnessNATS(t, h.NATSURL, "e2e-pt-selfservice-crw")
	startCredentialRequestWatcher(t, h.K8s, signer, idBaseURL, natsConn.Publish)

	// Provide a minimal LLM script: once the session un-parks, the
	// operator transitions it past AwaitingCredentials and spawns a
	// runner.Loop. Without scripted rules ScriptedLLM fatals on the
	// first unmatched request. The script is intentionally vacuous —
	// the test asserts only on cluster state, not on conversation.
	h.LLM.OnUserMessage("connect my linear").Reply(e2e.RespondToUser("ok"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	// Block until the AgentClass becomes Valid=True (the MCPServer
	// controller stamps it after a probe attempt — probe failure is
	// fine, finalSuccess stamps Valid unconditionally).
	h.WaitForAgentClassValid("triage-bot", 30*time.Second)

	// Step 1: drive the inbound that spawns the parked session.
	// SendUserMessage uses h.opts.DefaultUser (starterEmail) for the
	// channel-side identity; the pipeline canonicalizes it and stamps
	// AnnotationStartedByCanonicalID on the AgentSession.
	h.SendUserMessage("connect my linear")

	// Step 2: wait for the operator to park the session.
	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err)
	starterCanonical := starterCanon.Subject()
	t.Logf("starterCanonical=%s", starterCanonical)
	sess := waitForParkedSession(t, ctx, h.K8s, starterCanonical)

	// Step 2a: verify SUI + Explanation. The "no truncation" UX
	// contract: MissingCredentials enumerates every credential, and the
	// Explanation Items carry the user-facing Title (not the raw
	// credential name) for friendly rendering.
	var sui spiceboxv1alpha1.SessionUserIdentity
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKeyFromObject(sess), &sui),
		"get SessionUserIdentity %s/%s", sess.Namespace, sess.Name)
	assert.Equal(t, []string{linearCred}, sui.Status.MissingCredentials,
		"MissingCredentials must list every unlinked credential — no truncation")
	require.NotNil(t, sui.Status.Explanation, "Explanation should be stamped at park time")
	require.Len(t, sui.Status.Explanation.Items, 1, "one item per missing credential")
	assert.Equal(t, linearCred, sui.Status.Explanation.Items[0].Credential)
	assert.Equal(t, linearProvider, sui.Status.Explanation.Items[0].Title,
		"Explanation Item Title should carry the service label (%q)", linearProvider)

	// Step 3: wait for the credential_link interaction_request envelope to
	// land on the fake driver. The watcher polls every 5s; the first
	// reconcile fires at Run() start, then again every tick. Task 6 moved
	// delivery from a direct credential_request sub-channel send to a
	// NATS-published interaction_request — Driver.InteractionPrompts() is
	// where it lands now (Driver.CredentialRequests() is dead code post-flip:
	// nothing calls SubChannelSenderFor("credential_request") anymore).
	drv := fake.DriverFor(sess.Namespace, sess.Spec.InputChannel.Name)
	require.NotNil(t, drv, "fake.DriverFor(%s/%s) — listener may not have started",
		sess.Namespace, sess.Spec.InputChannel.Name)
	rec := waitForCredentialPrompt(t, ctx, drv, sess.Namespace+"/"+sess.Name)

	assert.Equal(t, sess.Namespace+"/"+sess.Name,
		rec.Payload.AgentSessionRef.Namespace+"/"+rec.Payload.AgentSessionRef.Name)
	require.NotNil(t, rec.Payload.Audience.Requester,
		"credential_link interaction_request must address a requester")
	reqCanon, canonErr := rec.Payload.Audience.Requester.Principal().AllowSynthetic().Canonical()
	require.NoError(t, canonErr, "credential_link requester canonical")
	assert.Equal(t, starterCanon, reqCanon, "credential_link interaction_request's requester is the starter")
	require.Len(t, rec.Payload.Actions, 1,
		"credential_link interaction_request carries exactly one connect action")
	linkAction := rec.Payload.Actions[0]
	assert.Equal(t, channelevents.ActionKindLink, linkAction.Kind)
	assert.True(t, strings.HasPrefix(linkAction.URL, idBaseURL+"/link?d="),
		"link action URL should be a fully-formed /link URL: got %q", linkAction.URL)
	assert.Contains(t, linkAction.URL, "&sig=",
		"link action URL should carry both d= and sig= query params: got %q", linkAction.URL)
	require.Len(t, rec.Payload.Fields, 1,
		"Fields has one entry per missing credential — no filtering")
	assert.Equal(t, linearProvider, rec.Payload.Fields[0].Label,
		"Fields[0].Label is bridged from the SUI Explanation Item Title — no filtering")

	// Step 4: drive the web flow.
	// 4a. Mint a cookie with the same signer identityd's OIDC callback
	// would have set. Subject must equal the link's Subject for the
	// /link cookie-gate to pass.
	cookieRaw, err := signer.Mint(passthroughlink.Payload{
		// identityd's checkOIDCCookie verifies iss=identityd aud=identityd
		// after the JWT-claims hardening; the cookie path used to be a
		// plain Subject-only payload but a fresh-minted cookie now needs
		// the same iss/aud the production setTrustLinkCookie writes.
		Issuer:    passthroughlink.IssuerIdentityd,
		Audience:  passthroughlink.AudienceIdentityd,
		Subject:   starterCanonical,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err, "mint idd_session cookie")
	cookie := &http.Cookie{Name: "idd_session", Value: cookieRaw}

	httpClient := &http.Client{
		// Don't follow redirects automatically — if the cookie gate
		// fails the handler 302s to /oidc/login, which a test would
		// rather see as "unexpected 302" than chase the sign-in flow.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 4b. GET /link with the cookie. Expected: 200 + the React identity-link
	// app mounted with bootstrap props that carry one row per missing
	// credential. The "no truncation" invariant: every required credential
	// name appears as a credentialName row in the bootstrap JSON exactly
	// once (the menu the user picks from to link).
	req, err := http.NewRequest(http.MethodGet, linkAction.URL, nil)
	require.NoError(t, err, "build GET /link request")
	req.AddCookie(cookie)
	resp, err := httpClient.Do(req)
	require.NoError(t, err, "GET /link")
	bodyGet := readAndCloseBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /link with valid cookie: status=%d body=%s", resp.StatusCode, bodyGet)
	// The page mounts the React identity-link app with the bootstrap JSON
	// the app reads: a per-credential row for each credential to link.
	assert.Contains(t, bodyGet, `data-app="identity-link"`,
		"/link should mount the identity-link React app: body=%s", bodyGet)
	assert.Contains(t, bodyGet, `"credentialName":"`+linearCred+`"`,
		"/link bootstrap should carry a row for %q: body=%s", linearCred, bodyGet)

	// 4c. POST /link/submit per-credential. The menu's per-row Save
	// form submits one credential at a time: hidden "link" carries
	// the signed deep-link, "credential" names the row, "token" holds
	// the value.
	parsed, err := url.Parse(linkAction.URL)
	require.NoError(t, err, "parse link action URL")
	rawSignedLink := parsed.Query().Get("d") + "." + parsed.Query().Get("sig")

	form := url.Values{
		"link":       {rawSignedLink},
		"credential": {linearCred},
		"token":      {patValue},
	}
	submitReq, err := http.NewRequest(http.MethodPost,
		idBaseURL+"/link/submit", strings.NewReader(form.Encode()))
	require.NoError(t, err, "build POST /link/submit request")
	submitReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	submitReq.AddCookie(cookie)
	// The handler redirects back to /link?d=&sig= so the user lands
	// on a refreshed menu with the row badge flipped to "Connected".
	// Use a no-redirect client so we observe the 302 directly.
	noRedirect := &http.Client{
		Timeout: httpClient.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	submitResp, err := noRedirect.Do(submitReq)
	require.NoError(t, err, "POST /link/submit")
	submitBody := readAndCloseBody(t, submitResp)
	require.Equal(t, http.StatusFound, submitResp.StatusCode,
		"POST /link/submit: status=%d body=%s", submitResp.StatusCode, submitBody)
	loc := submitResp.Header.Get("Location")
	assert.Contains(t, loc, "/link?d=",
		"redirect must land back on the menu page so the row badge updates")

	// Step 5: verify the cluster writes /link/submit produced.
	uiName := useridentity.NameForSubject(starterCanonical)
	masterSecretName := useridentity.MasterSecretName(uiName, linearCred)

	var masterSec corev1.Secret
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      masterSecretName,
	}, &masterSec), "get master Secret for %s/%s", uiName, linearCred)
	assert.Equal(t, []byte(patValue), masterSec.Data["token"],
		"master Secret data[token] should carry the submitted PAT verbatim")

	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Name: uiName}, &ui),
		"get UserIdentity %s", uiName)
	assert.Equal(t, starterCanonical.String(), ui.Spec.Subject,
		"UserIdentity.Spec.Subject should equal the link's Subject")
	require.Len(t, ui.Spec.Credentials, 1,
		"UserIdentity should have exactly one credential after a single submit")
	assert.Equal(t, linearCred, ui.Spec.Credentials[0].Name)
	require.NotNil(t, ui.Spec.Credentials[0].Static,
		"UserIdentity credential should be a static binding")
	assert.Equal(t, masterSecretName, ui.Spec.Credentials[0].Static.SecretRef.Name)

	// Step 6: poll until the operator un-parks. The operator watches
	// UserIdentity and re-enqueues the parked session; the passthrough
	// gate then sees the credential present, writes the SUID
	// Ready=True, and sets CredentialsReady=True on the session.
	waitForUnpark(t, ctx, h.K8s, sess.Namespace, sess.Name)

	// Step 7: post-unpark RBAC must be intact so the runner pod can
	// actually do work. Two regression guards for the failure mode a
	// real user hit: (a) the operator created the passthrough Role
	// (granting the runner SA update on master Secrets) so JIT OAuth
	// refresh works; (b) the per-session runner Role contains the
	// sessionuseridentities rule so MCP auth resolution succeeds.
	// Without either, the runner pod aborts with MCPAuthResolutionFailed
	// and the chat session goes silent.
	assertPostUnparkRBACComplete(t, ctx, h.K8s, sess.Namespace, sess.Name, linearCred, masterSecretName)
}

// assertPostUnparkRBACComplete checks the two Roles the operator must
// have created by the time the session un-parks:
//
//   - <session>-runner: per-session Role in the session's namespace
//     with a rule granting get on sessionuseridentities/<session>.
//     This is what lets the runner read its own SUI to map MCPServer
//     calls to user credentials in userPassthrough mode.
//
//   - <session>-passthrough-creds: per-session projected Secret in the
//     session namespace carrying each linked type=static credential's
//     VALUE (keyed by credential name), with a get-only reader Role
//     (<session>-passthrough-cred-reader). The projection is what closes
//     the cross-namespace ToolCall denial; no identities-namespace master
//     Role is created for an all-static session.
//
// If either shape regresses, the runner pod fails MCP auth resolution or
// a sandbox ToolCall is denied, and the chat session goes silent. Catches
// that specifically.
func assertPostUnparkRBACComplete(t *testing.T, ctx context.Context, c client.Client, ns, sessName, credName, masterSecretName string) {
	t.Helper()

	// (a) Per-session runner Role with SUI rule.
	var runnerRole rbacv1.Role
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: ns, Name: sessName + "-runner",
	}, &runnerRole), "runner Role must exist post-unpark")

	var suiRule *rbacv1.PolicyRule
	for i := range runnerRole.Rules {
		for _, res := range runnerRole.Rules[i].Resources {
			if res == "sessionuseridentities" {
				suiRule = &runnerRole.Rules[i]
				break
			}
		}
		if suiRule != nil {
			break
		}
	}
	require.NotNil(t, suiRule,
		"runner Role MUST contain a sessionuseridentities rule — without it MCPAuthResolutionFailed fires and the chat session goes silent post-link")
	assert.Contains(t, suiRule.Verbs, "get", "runner needs get on its SUI")
	assert.Equal(t, []string{sessName}, suiRule.ResourceNames,
		"SUI rule must be resourceName-scoped to the session's own name")

	// (b) type=static credentials are PROJECTED into a per-session Secret in the
	// SESSION namespace (not read cross-namespace from the master), guarded by a
	// get-only reader Role. This is what closes the cross-namespace ToolCall
	// denial: a sandbox ToolCall's credential source then resolves WITHIN the
	// ToolCall's own namespace. The link form stores user PATs as type=static.
	var projected corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: ns, Name: spiceboxv1alpha1.PassthroughCredentialSecretName(sessName),
	}, &projected), "per-session credential Secret must exist in the session namespace post-unpark")
	var masterSec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: masterSecretName,
	}, &masterSec), "get master Secret for credential %q", credName)
	assert.Equal(t, masterSec.Data["token"], projected.Data[credName],
		"projected value (keyed by credential name) must equal the master credential value")

	var readerRole rbacv1.Role
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: ns, Name: sessName + "-passthrough-cred-reader",
	}, &readerRole), "per-session credential reader Role must exist in the session namespace")
	require.Len(t, readerRole.Rules, 1)
	assert.Equal(t, []string{spiceboxv1alpha1.PassthroughCredentialSecretName(sessName)}, readerRole.Rules[0].ResourceNames,
		"reader Role must pin the per-session projected Secret")
	assert.Equal(t, []string{"get"}, readerRole.Rules[0].Verbs,
		"reader Role is get-only: the projected copy is never refreshed")

	// No identities-namespace master Role for an all-static session: the runner
	// no longer reads static masters cross-namespace (it reads the projected copy).
	var masterRole rbacv1.Role
	mErr := c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: "passthrough-" + sessName,
	}, &masterRole)
	if apierrors.IsNotFound(mErr) {
		var sess spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: sessName}, &sess))
		mErr = c.Get(ctx, client.ObjectKey{
			Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: "passthrough-" + string(sess.UID),
		}, &masterRole)
	}
	assert.True(t, apierrors.IsNotFound(mErr),
		"no identities-namespace master Role should exist when every linked credential is type=static")
}

// ----- helpers -----------------------------------------------------------

// startIdentityd brings up an in-process identityd HTTP server on a
// httptest.Server. Returns the externally reachable base URL.
//
// identityd's /link + /portal pages are React apps rendered through the
// webui framework: the page handlers read the framework renderer off the
// request context (webui.RendererFromContext) and emit an empty 200 if it
// is absent. So the harness mounts identityd's routes through a real
// webui.Server (exactly as internal/cmd/webd does in production) rather than
// calling identityd.Server.Handler() directly — only the framework path
// injects the renderer, so only it produces the React document.
//
// The chicken-and-egg: identityd needs the ExternalBaseURL up front (it's
// baked into authenticator Begin() URLs and cookie Secure flags), but
// httptest.NewServer's URL is only known after Start. Workaround: bind the
// listener first via NewUnstartedServer (the URL is then computable as
// http://<addr>:<port>), construct the webui.Server with a host getter
// reading that URL, then point the test server's Handler at it.
//
// The fake WebAuthenticator is registered so that — if the cookie path
// ever fails and the handler redirects to /oidc/login (which dispatches
// the session kind's authenticator) — the failure mode is a recognizable
// test signal, not a 404 panic.
func startIdentityd(t *testing.T, c client.Client, signer *passthroughlink.Signer) string {
	t.Helper()

	// An unstarted httptest server so its URL is known before we build the
	// webui.Server (whose trusted-host getter dispatches on that host). The
	// handler closure points at the real framework handler once it exists.
	var srv http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
	}))
	ts.Start()
	t.Cleanup(ts.Close)

	fakeAuth := (fake.Kind{}).WebAuthenticator(channelkinds.WebAuthDeps{ExternalBaseURL: ts.URL})
	deps := harnessWebDeps{
		k8s:             c,
		linkSigner:      signer,
		externalBaseURL: func() string { return ts.URL },
		authenticators: map[string]channelkinds.WebAuthenticator{
			"fake": fakeAuth,
		},
	}
	// Mount identityd's routes on a real webui.Server, mirroring internal/cmd/webd.
	// The framework auth gate is satisfied by a permissive authenticate
	// func — identityd's handlers do their OWN idd_session cookie gating
	// internally (the gate these scenarios exercise), so a permissive
	// framework subject just lets the request reach the handler. The
	// trusted-host getter returns the httptest host so ServeHTTP's Host
	// dispatch routes to identityd's (trusted-origin) routes; the sandbox
	// host is unused here.
	host := func() string { return ts.URL }
	ws, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "framework-subject", true },
		nil,
		host,
		func() string { return "sandbox.invalid" },
		nil,
		deps,
		[]webui.WebUI{identityd.New()},
	)
	require.NoError(t, err, "mount identityd on the webui framework")
	srv = ws

	return ts.URL
}

// harnessWebDeps satisfies identityd.WebDeps so the webui framework can
// mount identityd's routes against the harness's fake K8s client + signer.
// Mirrors internal/cmd/webd's webdDeps (the production implementation).
type harnessWebDeps struct {
	k8s             client.Client
	linkSigner      *passthroughlink.Signer
	externalBaseURL func() string
	authenticators  map[string]channelkinds.WebAuthenticator
}

func (d harnessWebDeps) K8s() client.Client                  { return d.k8s }
func (d harnessWebDeps) LinkSigner() *passthroughlink.Signer { return d.linkSigner }
func (d harnessWebDeps) ExternalBaseURL() string             { return d.externalBaseURL() }
func (d harnessWebDeps) IconHandler() http.Handler           { return nil }
func (d harnessWebDeps) Authenticators() map[string]channelkinds.WebAuthenticator {
	return d.authenticators
}

// InsecureTrustLinks enables the legacy trust-the-link fallback the
// passthrough scenarios were written against (no channel authenticator
// is wired in the harness).
// False: every /link request in this scenario carries a pre-minted
// idd_session cookie (issued as identityd's OIDC callback would), so
// the InsecureTrustLinks path is never taken.
func (d harnessWebDeps) InsecureTrustLinks() bool { return false }

// startCredentialRequestWatcher constructs a pipeline.CredentialRequestWatcher
// pointed at the test's signer + identityd base URL, then runs it in a
// goroutine bound to its own context. The watcher's poll loop (5s ticks
// plus an initial run) reaches the parked session within one tick.
//
// The senderResolver is a minimal adapter — same code path the
// production internal/cmd/channelsd/sender_resolver and the e2e harness's
// e2eSenderResolver use, but inline here so the test owns the seam
// between the watcher and the fake kind's sub-channel sender. Since Task 6's
// flip it is unused by the watcher's own publish path (retained on the
// struct per CredentialRequestWatcher.Senders's doc comment); natsPublish is
// what actually delivers now — see dialHarnessNATS.
//
// Owning the context here (instead of inheriting from the caller's)
// keeps the cleanup self-contained: the t.Cleanup cancels AND drains
// the goroutine in one shot, regardless of LIFO ordering relative to
// other cleanups.
//
// Returns the watcher so a caller that also needs the re-surface leg (see
// resurface_credential_test.go) can bind w.ForcePublish as the
// credential_link channelinteractions.Regenerator, mirroring
// internal/cmd/channelsd/main.go's wiring. Callers that only need the first-publish
// leg (every other scenario in this package) simply ignore the return value.
func startCredentialRequestWatcher(
	t *testing.T,
	c client.Client,
	signer *passthroughlink.Signer,
	idBaseURL string,
	natsPublish channelevents.PublishFunc,
) *pipeline.CredentialRequestWatcher {
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
	return w
}

// dialHarnessNATS opens a fresh *nats.Conn against the e2e harness's own
// embedded NATS server (h.NATSURL). The harness's outbound relay already
// subscribes to ap.session.*.*.out.> on its own (unexported) connection —
// scenario tests can't reach that connection directly, but NATS pub/sub
// works fine across separate client connections to the same embedded
// server, so a watcher publishing on this connection is still picked up
// and routed to the fake kind's "interaction" sub-channel sender. Mirrors
// the dial pattern in test/e2e/scenarios/passthrough_revoke_propagation.
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
// against the channel registry. Mirrors e2eSenderResolver.SubChannelSenderFor
// inline so the test doesn't reach into harness internals.
type testSubChannelResolver struct {
	c client.Client
}

func (r *testSubChannelResolver) SubChannelSenderFor(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	name string,
) (channelkinds.Sender, error) {
	if sess.Spec.InputChannel == nil {
		return nil, fmt.Errorf("session %s/%s has no InputChannel", sess.Namespace, sess.Name)
	}
	ch, sec, k, err := resolve.ForSession(ctx, r.c, sess)
	if err != nil {
		return nil, fmt.Errorf("resolve channel for %s/%s: %w", sess.Namespace, sess.Name, err)
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
// namespace. envtest doesn't auto-create namespaces, and
// useridentity.PutToken's master-Secret Create lands there.
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
// annotation. Returns the parked session. Fatals on timeout, with a
// diagnostic of what the last-observed session looked like.
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
// Returns the recorded entry.
//
// Task 6 flipped CredentialRequestWatcher from a direct
// SubChannelSenderFor("credential_request") send to a NATS-published
// interaction_request(credential_link) envelope, routed by the outbound
// relay to the fake kind's "interaction" sub-channel sender —
// Driver.InteractionPrompts() is where it lands now.
// Driver.CredentialRequests() (the pre-flip queue) is dead: nothing calls
// SubChannelSenderFor("credential_request") anymore.
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

// waitForUnpark polls until both the session's CredentialsReady=True and
// the SessionUserIdentity Ready=True. The operator clears
// MissingCredentials AT THE SAME TIME it sets Ready=True (single Patch),
// so a single condition check covers both.
//
// The session phase is NOT reset by the operator (the runner writes the
// next phase); waiting for Phase != AwaitingCredentials is the spec's
// secondary signal, asserted alongside.
func waitForUnpark(t *testing.T, ctx context.Context, c client.Client, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var sess spiceboxv1alpha1.AgentSession
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err == nil {
			credReady := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
			if credReady != nil && credReady.Status == metav1.ConditionTrue &&
				sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials {
				// Cross-check SUID is also Ready=True.
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
// from the rest of the multi-doc YAML. Returns (mcpServerDoc,
// remainingDocs). Mirrors the helper in test/e2e/scenarios/credentials —
// the MCPServer's spec.server.url contains the {{MCP_URL}} sentinel
// substituted with h.MCP.URL() after the harness has started.
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

// readAndCloseBody reads + closes the response body. Returns the body
// as a string for easier substring assertions. Test fails on read
// error so the caller's assert.Contains doesn't run on a half-read
// body.
func readAndCloseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return string(b)
}
