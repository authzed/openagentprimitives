//go:build e2e

// Package passthrough_portal_test is the Slice-2.5 phase-δ end-to-end
// scenario for the proactive-link / portal flow. It drives every moving
// part of the
//
//	"chat trigger → channelsd publishes portal_access → identityd /my/accounts
//	 bootstrap → /my/accounts/<cred>/link form → /link/submit persists →
//	 channelsd's credential_linked watcher publishes OOB confirmation"
//
// chain in a single in-process process.
//
// What this proves end-to-end:
//
//  1. A user-typed portal trigger phrase ("manage my accounts") on the
//     bound channel routes through channelsd's pipeline + α5's
//     PortalAccessTriggerer; the message is consumed (agent never sees
//     it) and an interaction_request(portal_access) envelope with a signed
//     portal-link (as the request's single ActionKindLink action) is
//     published to the fake kind's "interaction" sub-channel sender. Task 6
//     flipped the triggerer off a direct KindPortalAccess send onto this
//     generic-Interaction publish path — see
//     pkg/channels/channelsd/pipeline/portal_access.go's file doc comment.
//
//  2. identityd's GET /my/accounts handler verifies the portal link,
//     sets the idd_session cookie via the trust-link bootstrap path
//     (no OIDC redirect), and renders the portal page listing every
//     passthrough AgentClass's credentials as "Suggested". The
//     credential is sourced from a SEPARATE passthrough AgentClass
//     (linear-helper) — the bound channel uses a non-passthrough class
//     (triage-bot) so the session stays active enough for the portal
//     trigger to find it.
//
//  3. The follow-up GET /my/accounts/<cred>/link renders the
//     one-credential PAT form; the matching POST /link/submit persists
//     the credential via useridentity.PutToken, which creates the
//     master Secret in agentprimitives-identities + populates
//     UserIdentity.Spec.Credentials.
//
//  4. channelsd's credential_linked watcher (ι3) diffs UserIdentity
//     spec.credentials against its in-memory snapshot, observes the
//     just-linked credential as ADDED, and publishes a
//     KindCredentialLinked envelope on the user's most recent
//     AgentSession's channel (the same fake channel the portal trigger
//     fired on).
//
// The cookie jar threads the idd_session cookie across all three
// portal-side HTTP requests; without it the GET form + POST submit
// would 401 (cookie-gated) and the test would surface as
// "/my/accounts/<cred>/link: 401" — a recognizable signal that the
// trust-link bootstrap path didn't set the cookie.
//
// No real names: alice / Triage Bot / Linear Helper / example.com are
// all fictional per AGENTS.md.
package passthrough_portal_test

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
	// starterEmail is the human the test impersonates. Mapped to a
	// canonical SpiceDB subject via identity.Principal.Canonical() at the
	// pipeline boundary.
	starterEmail = "alice@example.com"

	// linearCred is the credential identityd's portal lists as
	// "Suggested" (sourced from the linear-helper passthrough
	// AgentClass's MCPServer). The test clicks Link on it, submits a
	// PAT, and asserts the credential lands on the UserIdentity.
	linearCred = "linear-oauth"

	// linearProvider is the user-visible label resolved from the
	// MCPServer's auth.provider; surfaces both on the portal page and
	// on the link form.
	linearProvider = "Linear"

	// portalTrigger is one of α5's three hardcoded portal-trigger
	// phrases (matchesPortalTrigger). The test could use any of them;
	// "manage my accounts" is the canonical one named in the user-facing
	// docs.
	portalTrigger = "manage my accounts"

	// primeMessage is a non-trigger message used to create the active
	// session before the portal trigger fires. The portal triggerer only
	// fires on inbound that routes to an existing active session
	// (Pending/Running/Idle), so a first message must seed the session.
	primeMessage = "hello"

	// patValue is the bearer-token value the test posts to the portal's
	// /link/submit endpoint. Hex chosen to avoid accidental matches with
	// other env vars or fixture strings.
	patValue = "pat-e2e-portal-token-9f8e7d6c5b4a"

	// signingKey is the 32-byte HMAC key SHARED by the in-test
	// PortalAccessTriggerer (channelsd side, mints the link) and the
	// in-test identityd (verifies the link + sets the cookie). Both ends
	// of the mint/verify boundary use it.
	signingKey = "passthrough-portal-test-key-12345" // 32 bytes
)

// TestPassthroughPortal is the end-to-end assertion for Slice 2.5 phase
// α + ι3. One linear test body so a failure surfaces the step that
// broke (each section logs the assertion intent before driving it).
func TestPassthroughPortal(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	// Split the MCPServer doc out so it can be applied AFTER the harness's
	// MCPStub URL is known. The MCPServer's spec.server.url must resolve
	// from the in-process MCPStub or the MCPServer controller's probe
	// would hang on DNS for the probe-timeout window every reconcile.
	rawManifests := string(manifests)
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(rawManifests)

	// Two signers, one HMAC key. Mirrors production where channelsd
	// and identityd each construct their own Signer (same key, different
	// iss). Tests for other passthrough scenarios get away with one
	// signer because they mint the cookie directly (and can override
	// Issuer at the call site), but portal exercises identityd's
	// setTrustLinkCookie bootstrap (GET /my/accounts with a portal
	// link), which Mints with the signer's default iss — so the test
	// can't override per-call and needs a properly-configured
	// identityd-side signer.
	linkSigner := passthroughlink.New([]byte(signingKey),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))
	idSigner := passthroughlink.New([]byte(signingKey),
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	// triggerer is the PortalAccessTriggerer the PipelineExtender
	// attaches to the harness's in-process pipeline. We keep a
	// reference here so the test body can rewire its ExternalBaseURL
	// after identityd starts (the identityd httptest.Server's URL is
	// only known post-Start, but PipelineExtender runs INSIDE the
	// harness's Start, before identityd is bound). The triggerer reads
	// ExternalBaseURL at TryHandle time, so mutating it before the
	// first portal-trigger inbound is sufficient.
	var triggerer *pipeline.PortalAccessTriggerer

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    starterEmail,
		// PipelineExtender wires the PortalAccessTriggerer onto the
		// harness's in-process channelsd pipeline. The default harness
		// pipeline (Slice 2's E2E) leaves PortalAccess nil; this
		// scenario needs it because the trigger fires inside
		// Pipeline.Deliver and there is no way to inject it from
		// outside the harness's startChannelsdPlumbing without an
		// extension hook.
		PipelineExtender: func(pl *pipeline.Pipeline, view e2e.ExtenderView) {
			triggerer = &pipeline.PortalAccessTriggerer{
				LinkSigner: linkSigner,
				// ExternalBaseURL is filled in below once identityd
				// has bound; left empty here. The triggerer's
				// "missing config" branch consumes the message
				// silently if invoked with an empty URL, so the test
				// body MUST set it before the first portal trigger.
				ExternalBaseURL: func() string { return "" },
				Senders:         &testSubChannelResolver{c: view.K8s},
				// NATS is required post Task 6's flip: TryHandle now
				// publishes the interaction_request(portal_access) envelope
				// via NATS.Publish rather than a direct sub-channel send —
				// mirrors internal/cmd/channelsd/main.go's wiring (NATS: pl.NATS).
				// A nil NATS makes TryHandle silently consume the trigger
				// message with no publish (its "missing config" branch),
				// which is exactly what made this scenario time out with
				// zero recorded interaction prompts before this fix.
				NATS: pl.NATS,
			}
			pl.PortalAccess = triggerer
		},
	})
	require.NotNil(t, triggerer, "PipelineExtender must have run during e2e.Start")

	// Apply the MCPServer now that h.MCP.URL() is known.
	h.ApplyManifest(strings.ReplaceAll(mcpServerDoc, "{{MCP_URL}}", h.MCP.URL()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// envtest doesn't auto-create namespaces; useridentity.PutToken
	// (called by /link/submit) needs the agentprimitives-identities
	// namespace to exist for its master-Secret Create.
	createIdentitiesNamespace(t, ctx, h.K8s)

	// Start identityd on a httptest.Server. identityd uses idSigner so
	// its setTrustLinkCookie Mints cookies with iss=identityd (required
	// by checkOIDCCookie). Link verification still works because the
	// HMAC key is shared with linkSigner.
	idBaseURL := startIdentityd(t, h.K8s, idSigner)
	t.Logf("identityd: %s", idBaseURL)

	// Rewire the triggerer's ExternalBaseURL now that the real URL is
	// known. Safe to mutate: PortalAccessTriggerer reads the field
	// inside TryHandle, no caller holds it before the first trigger.
	triggerer.ExternalBaseURL = func() string { return idBaseURL }

	// Pre-create an EMPTY UserIdentity for the starter. The watcher's
	// "brand-new UserIdentity observed post-startup" branch primes
	// without emitting (intentional in production to avoid spam on
	// channelsd restart). Pre-creating the UserIdentity with no
	// credentials and letting the watcher prime observed[name]=∅ means
	// the subsequent PutToken (which appends linear-oauth to
	// Spec.Credentials) registers as ADDED → emits the confirmation.
	// Without this, the watcher would prime the just-created
	// UserIdentity with one credential already present and treat the
	// initial state as the baseline.
	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err)
	starterCanonical := starterCanon.Subject()
	createEmptyUserIdentity(t, ctx, h.K8s, starterCanonical)

	// Start the credential_linked watcher. The harness doesn't wire it
	// by default (production wires it in internal/cmd/channelsd/main.go's ι3
	// section); the scenario needs it for the OOB confirmation
	// assertion at the end. A fast PollInterval keeps the test's
	// Eventually deadline tight; the default 5s would push the test
	// timeout uncomfortably close to the harness DefaultTimeout.
	//
	// Task 6 flipped the watcher's emit() from a direct credential_linked
	// sub-channel send to a NATS-published interaction_applied(resolved) —
	// dial a connection against the harness's embedded NATS server so the
	// publish reaches the harness's already-running outbound relay (see
	// dialHarnessNATS).
	natsConn := dialHarnessNATS(t, h.NATSURL, "e2e-pt-portal-clw")
	startCredentialLinkedWatcher(t, h.K8s, 500*time.Millisecond, natsConn.Publish)

	// Provide a minimal LLM script: once the session is created by the
	// first message, the operator transitions it past Pending and
	// spawns a runner.Loop. Without scripted rules ScriptedLLM fatals
	// on the first unmatched request.
	h.LLM.OnUserMessage(primeMessage).Reply(e2e.RespondToUser("hi"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	// Block until the bound AgentClass becomes Valid=True.
	h.WaitForAgentClassValid("triage-bot", 30*time.Second)

	// Wait for the fake listener to register its Driver. The harness's
	// listener starter polls Channels every 250ms; by the time the
	// AgentClass is Valid the listener has usually started, but
	// WaitForAgentClassValid does not gate on listener startup
	// explicitly. Poll until the Driver appears so the first
	// SendUserMessage doesn't race the listener starter.
	{
		ch := singleChannel(t, ctx, h.K8s)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if fake.DriverFor(ch.Namespace, ch.Name) != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		require.NotNil(t, fake.DriverFor(ch.Namespace, ch.Name),
			"fake.Driver for %s/%s should be registered by listener start within 15s",
			ch.Namespace, ch.Name)
	}

	// Step 1: prime the session. The first message creates an active
	// AgentSession the portal triggerer can find on the second message.
	h.SendUserMessage(primeMessage)
	// Wait for the agent's reply so the session is in a stable state
	// before the portal trigger fires. ExpectAgentReply blocks until
	// the outbound user_message envelope hits the fake driver — by
	// then the session has been created, the runner ran, and the
	// session is heading toward Idle.
	h.ExpectAgentReply(e2e.Contains("hi"))

	t.Logf("starterCanonical=%s", starterCanonical)

	// Step 2: send the portal trigger phrase. The PortalAccessTriggerer
	// matches the phrase, mints a portal-link, and publishes an
	// interaction_request(portal_access) envelope on the fake channel's
	// "interaction" sub-channel sender — without waking the agent.
	h.SendUserMessage(portalTrigger)

	// Step 3: wait for the portal-access interaction_request to land. The
	// PortalAccessTriggerer publishes synchronously inside Deliver, so
	// this is a quick poll.
	ch := singleChannel(t, ctx, h.K8s)
	drv := fake.DriverFor(ch.Namespace, ch.Name)
	require.NotNil(t, drv, "fake.DriverFor(%s/%s) — listener may not have started",
		ch.Namespace, ch.Name)
	prompt := waitForPortalAccessPrompt(t, ctx, drv)
	require.NotNil(t, prompt.Payload.Audience.Requester,
		"portal_access interaction_request should address a requester")
	reqCanon, canonErr := prompt.Payload.Audience.Requester.Principal().AllowSynthetic().Canonical()
	require.NoError(t, canonErr, "portal_access requester canonical")
	assert.Equal(t, starterCanon, reqCanon, "portal_access interaction_request's requester is the starter")
	require.Len(t, prompt.Payload.Actions, 1,
		"portal_access interaction_request should carry exactly one action")
	action := prompt.Payload.Actions[0]
	assert.Equal(t, channelevents.ActionKindLink, action.Kind,
		"portal_access action should be a link action")
	linkURL := action.URL
	assert.True(t, strings.HasPrefix(linkURL, idBaseURL+"/my/accounts?d="),
		"LinkURL should target identityd's /my/accounts with d=: got %q", linkURL)
	assert.Contains(t, linkURL, "&sig=",
		"LinkURL should carry both d= and sig= query params: got %q", linkURL)

	// Step 4: drive the web flow with a cookie jar so the idd_session
	// cookie set by GET /my/accounts persists to the form GET and the
	// submit POST.
	jar, err := cookiejar.New(nil)
	require.NoError(t, err, "new cookie jar")
	httpClient := &http.Client{
		Jar: jar,
		// Don't follow redirects automatically — if any handler
		// surprises us with a 302 (e.g. cookie gate fails), a redirect
		// chase would obscure the real failure.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 4a. GET <LinkURL>. Trust-link bootstrap path: identityd verifies
	// the link, sets the idd_session cookie, renders the portal page
	// listing linear-oauth as a suggested credential.
	req, err := http.NewRequest(http.MethodGet, linkURL, nil)
	require.NoError(t, err, "build GET /my/accounts request")
	resp, err := httpClient.Do(req)
	require.NoError(t, err, "GET /my/accounts")
	body := readAndCloseBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /my/accounts with portal link: status=%d body=%s", resp.StatusCode, body)
	// Cookie should now be set on the jar.
	cookieURL, _ := url.Parse(idBaseURL)
	cookies := jar.Cookies(cookieURL)
	hasIDDSession := false
	for _, c := range cookies {
		if c.Name == "idd_session" {
			hasIDDSession = true
			break
		}
	}
	assert.True(t, hasIDDSession, "GET /my/accounts must set the idd_session cookie")
	// The portal is now the React identity-portal app; the handler emits the
	// document mounting it + the JSON bootstrap. The credential appears as a
	// row in the "suggested" bootstrap list (the React app builds the
	// /my/accounts/<cred>/link href from it). Assert the app mounts, the
	// suggested section is present, and the credential name + provider label
	// are carried — a regression in either path is easy to spot.
	assert.Contains(t, body, `data-app="identity-portal"`,
		"portal page should mount the identity-portal React app: body=%s", body)
	assert.Contains(t, body, `"suggested":`,
		"portal bootstrap should carry the suggested-credentials section: body=%s", body)
	assert.Contains(t, body, `"name":"`+linearCred+`"`,
		"portal bootstrap should list %q as a suggested credential: body=%s", linearCred, body)
	assert.Contains(t, body, `"label":"`+linearProvider+`"`,
		"portal bootstrap should carry the provider label %q: body=%s", linearProvider, body)

	// 4b. GET /my/accounts/<cred>/link. One-credential form page.
	formURL := idBaseURL + "/my/accounts/" + linearCred + "/link"
	req2, err := http.NewRequest(http.MethodGet, formURL, nil)
	require.NoError(t, err, "build GET /my/accounts/<cred>/link request")
	resp2, err := httpClient.Do(req2)
	require.NoError(t, err, "GET /my/accounts/<cred>/link")
	formBody := readAndCloseBody(t, resp2)
	require.Equal(t, http.StatusOK, resp2.StatusCode,
		"GET /my/accounts/%s/link: status=%d body=%s", linearCred, resp2.StatusCode, formBody)
	// The PAT form is now the React identity-link-form app; the handler emits
	// the document mounting it + the credential-name bootstrap the form posts
	// for. The form's POST target (/my/accounts/<cred>/link/submit) is built
	// by the React app from credentialName.
	assert.Contains(t, formBody, `data-app="identity-link-form"`,
		"link form should mount the identity-link-form React app: body=%s", formBody)
	assert.Contains(t, formBody, `"credentialName":"`+linearCred+`"`,
		"link-form bootstrap should carry the credential %q the form posts for: body=%s", linearCred, formBody)

	// 4c. POST /my/accounts/<cred>/link/submit with the PAT. identityd calls
	// useridentity.PutToken under the hood; success is now Post/Redirect/Get:
	// a 303 back to the portal with a one-time linked notice the React portal
	// surfaces as a banner.
	submitURL := idBaseURL + "/my/accounts/" + linearCred + "/link/submit"
	form := url.Values{"token": {patValue}}
	req3, err := http.NewRequest(http.MethodPost, submitURL, strings.NewReader(form.Encode()))
	require.NoError(t, err, "build POST /my/accounts/<cred>/link/submit request")
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp3, err := httpClient.Do(req3)
	require.NoError(t, err, "POST /my/accounts/<cred>/link/submit")
	submitBody := readAndCloseBody(t, resp3)
	require.Equal(t, http.StatusSeeOther, resp3.StatusCode,
		"POST /my/accounts/%s/link/submit success → 303 redirect: status=%d body=%s", linearCred, resp3.StatusCode, submitBody)
	assert.Equal(t, "/my/accounts?notice=linked:"+linearCred, resp3.Header.Get("Location"),
		"link/submit must redirect back to the portal with a linked notice for %q", linearCred)

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
		"UserIdentity.Spec.Subject should equal the cookie subject")
	require.Len(t, ui.Spec.Credentials, 1,
		"UserIdentity should have exactly one credential after a single submit")
	assert.Equal(t, linearCred, ui.Spec.Credentials[0].Name)
	require.NotNil(t, ui.Spec.Credentials[0].Static,
		"UserIdentity credential should be a static binding")
	assert.Equal(t, masterSecretName, ui.Spec.Credentials[0].Static.SecretRef.Name)

	// Step 6: assert the OOB credential_linked confirmation was published.
	// Task 6 flipped CredentialLinkedWatcher.emit from a direct
	// credential_linked sub-channel send to a NATS-published
	// interaction_applied(category=credential_link, outcome=resolved) —
	// Driver.InteractionApplieds() is where it lands now
	// (Driver.CredentialLinkeds() is dead post-flip: nothing calls
	// SubChannelSenderFor("credential_linked") anymore). The watcher
	// diff-emits on its next tick after the UserIdentity gains the
	// credential; with PollInterval=500ms the first tick after PutToken
	// should fire within ~1s.
	credentialLinkedApplieds := func() []fake.InteractionApplied {
		var out []fake.InteractionApplied
		for _, a := range drv.InteractionApplieds() {
			if a.Payload.Category == categories.CredentialLink {
				out = append(out, a)
			}
		}
		return out
	}
	require.Eventually(t, func() bool {
		return len(credentialLinkedApplieds()) >= 1
	}, 15*time.Second, 100*time.Millisecond,
		"credential_link interaction_applied should appear within 15s of /link/submit")
	linked := credentialLinkedApplieds()
	require.Len(t, linked, 1,
		"exactly one credential_link interaction_applied expected for one PutToken")
	assert.Equal(t, channelevents.OutcomeResolved, linked[0].Payload.Outcome,
		"credential_link interaction_applied outcome should be resolved (out-of-band confirmation)")
	assert.Equal(t, linearCred, linked[0].Payload.RequestRef,
		"credential_link interaction_applied should name the just-linked credential via RequestRef")
	assert.Equal(t, linearCred, linked[0].Payload.OutcomeText,
		"credential_link interaction_applied OutcomeText should carry the credential name")
	// The new Interaction model routes per-session/per-channel rather than
	// carrying a RecipientCanonical on the payload; the closest available
	// "went to the right place" check is that the envelope's AgentSessionRef
	// names the same session the portal flow drove.
	primedSess := singleSession(t, ctx, h.K8s)
	assert.Equal(t, primedSess.Namespace+"/"+primedSess.Name,
		linked[0].Payload.AgentSessionRef.Namespace+"/"+linked[0].Payload.AgentSessionRef.Name,
		"credential_link interaction_applied should target the session the portal flow drove")
}

// ----- helpers -----------------------------------------------------------

// startIdentityd brings up an in-process identityd HTTP server on a
// httptest.Server. Returns the externally reachable base URL.
//
// identityd's /link + /portal pages are React apps rendered through the
// webui framework: the page handlers read the framework renderer off the
// request context and emit an empty 200 if it is absent. So the harness
// mounts identityd's routes through a real webui.Server (as internal/cmd/webd does
// in production), not identityd.Server.Handler() directly — only the
// framework path injects the renderer that produces the React document.
//
// Chicken-and-egg: identityd needs ExternalBaseURL up front, but
// httptest.NewServer's URL is only known after Start. We bind an unstarted
// server first (the URL becomes computable), wire the handler closure,
// then point the test server at the real framework handler once built.
//
// The fake WebAuthenticator is registered so a redirect to /oidc/login
// (which shouldn't happen on the trust-link path) surfaces as a
// recognizable test signal rather than a 404 panic.
func startIdentityd(t *testing.T, c client.Client, signer *passthroughlink.Signer) string {
	t.Helper()

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
	// Permissive framework auth gate; identityd does its own idd_session
	// cookie gating internally. Trusted-host getter routes Host dispatch
	// to identityd's routes (sandbox unused).
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
// False: the portal scenario bootstraps the idd_session cookie via
// GET /my/accounts?d=&sig= (the portal-link path), which sets the
// cookie directly without consulting InsecureTrustLinks.
func (d harnessWebDeps) InsecureTrustLinks() bool { return false }

// startCredentialLinkedWatcher constructs a pipeline.CredentialLinkedWatcher
// pointed at the harness's k8s client, then runs it in a goroutine bound
// to its own context. PollInterval is small (500ms by default) so the
// test's Eventually deadline can be tight.
//
// Owning the context here keeps cleanup self-contained: t.Cleanup
// cancels AND waits for the goroutine to exit in one shot.
func startCredentialLinkedWatcher(t *testing.T, c client.Client, pollInterval time.Duration, natsPublish channelevents.PublishFunc) {
	t.Helper()
	w := &pipeline.CredentialLinkedWatcher{
		K8s:          c,
		Senders:      &testSubChannelResolver{c: c},
		PollInterval: pollInterval,
		NATSPublish:  natsPublish,
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
			t.Logf("credential_linked watcher did not exit within 2s")
		}
	})
}

// dialHarnessNATS opens a fresh *nats.Conn against the e2e harness's own
// embedded NATS server (h.NATSURL). Task 6 flipped CredentialLinkedWatcher
// from a direct SubChannelSenderFor send to a NATS publish of
// interaction_applied(credential_link); this connection is what lets that
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

// createEmptyUserIdentity creates a cluster-scoped UserIdentity for the
// starter with NO credentials. Used to ensure the credential_linked
// watcher primes a baseline (empty Spec.Credentials) before the portal
// flow's PutToken appends linear-oauth. Without this, the watcher's
// "brand-new UserIdentity post-startup" branch primes the existing
// (already-populated) credential set as the baseline and never emits
// the confirmation envelope.
//
// This mirrors the realistic production flow: a user's UserIdentity
// exists for some time (created by a prior credential link or by an
// administrator) before they link a new credential. The credential_linked
// watcher's purpose is to alert the user when ANY new credential is
// added to their existing identity — testing a "new identity AND new
// credential in the same scan" path would require a different watcher
// design.
func createEmptyUserIdentity(t *testing.T, ctx context.Context, c client.Client, subject identity.Subject) {
	t.Helper()
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(subject)},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     subject.String(),
			Credentials: []spiceboxv1alpha1.AgentCredential{},
		},
	}
	if err := c.Create(ctx, ui); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create empty UserIdentity %q: %v", ui.Name, err)
	}
}

// createIdentitiesNamespace creates the agentprimitives-identities
// namespace; envtest doesn't auto-create namespaces, and
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

// singleChannel returns the single Channel CR in the namespace; mirrors
// the harness's internal singleChannel but lives in the scenario so the
// test can use it directly.
func singleChannel(t *testing.T, ctx context.Context, c client.Client) *spiceboxv1alpha1.Channel {
	t.Helper()
	var channels spiceboxv1alpha1.ChannelList
	require.NoError(t, c.List(ctx, &channels), "list Channels")
	require.Len(t, channels.Items, 1, "scenario assumes one Channel CR")
	return &channels.Items[0]
}

// singleSession returns the single AgentSession CR in the cluster; mirrors
// singleChannel. Used to confirm an out-of-band interaction_applied
// envelope's AgentSessionRef targeted the session the scenario drove.
func singleSession(t *testing.T, ctx context.Context, c client.Client) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, c.List(ctx, &sessions), "list AgentSessions")
	require.Len(t, sessions.Items, 1, "scenario assumes one AgentSession")
	return &sessions.Items[0]
}

// waitForPortalAccessPrompt polls the Driver's InteractionPrompts queue
// until an entry with Category == categories.PortalAccess is recorded;
// returns the first match. Task 6 flipped the portal-access publisher off a
// direct KindPortalAccess sub-channel send onto the generic
// interaction_request(portal_access) publish path (see
// pkg/channels/channelsd/pipeline/portal_access.go's file doc comment), so the
// envelope now lands on the same "interaction" sub-channel sender every
// other migrated category shares — fake.Driver.PortalAccesses() (the
// pre-migration KindPortalAccess recorder) was removed as dead code once
// this scenario was the last caller. The PortalAccessTriggerer publishes
// synchronously inside Deliver, so this is effectively a sub-second wait —
// the timeout is loose to insulate against listener-startup races.
func waitForPortalAccessPrompt(t *testing.T, ctx context.Context, drv *fake.Driver) fake.InteractionPrompt {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range drv.InteractionPrompts() {
			if p.Payload.Category == categories.PortalAccess {
				return p
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waitForPortalAccessPrompt: context cancelled before envelope arrived")
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("waitForPortalAccessPrompt: no portal_access interaction_request within 30s (recorded %d interaction prompt(s) total)",
		len(drv.InteractionPrompts()))
	return fake.InteractionPrompt{}
}

// splitMCPServerFromManifests separates the MCPServer YAML document
// from the rest of the multi-doc YAML so the test can substitute the
// {{MCP_URL}} sentinel before applying it. Mirrors the helper in Slice
// 2's passthrough_selfservice scenario.
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
// as a string for easier substring assertions.
func readAndCloseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return string(b)
}
