//go:build e2e

// Package passthrough_revoke_test is the Slice-2.5 phase-δ2 end-to-end
// scenario for the revoke + re-link cycle. It drives every moving part
// of the
//
//	"revoke credential → session re-parks → channelsd credential_request →
//	 /link + /link/submit re-links → session un-parks →
//	 exactly one credential_linked OOB confirmation (the re-link; not the revoke)"
//
// chain in a single in-process process.
//
// What this proves end-to-end:
//
//  1. A pre-linked credential (seeded via useridentity.PutToken before
//     the test) can be revoked via identityd's POST
//     /my/accounts/<cred>/revoke; the master Secret is deleted and the
//     UserIdentity entry is removed.
//
//  2. After revocation, a new AgentSession for the same user parks in
//     AwaitingCredentials because the passthrough gate sees the credential
//     is missing again.
//
//  3. channelsd's CredentialRequestWatcher (E1) picks up the parked
//     session, mints a deep-link, and publishes a KindCredentialRequest
//     envelope on the fake channel's credential_request sub-channel.
//
//  4. The test re-drives the /link + /link/submit web flow (same as
//     Slice 2's passthrough_selfservice scenario) to re-link the
//     credential.
//
//  5. The operator's UserIdentity watch re-enqueues the parked session;
//     the passthrough gate un-parks it (CredentialsReady=True,
//     SessionUserIdentity Ready=True).
//
//  6. Per ι3's design, the revoke does NOT emit a credential_linked
//     envelope (only additions / replacements emit). The re-link DOES
//     emit one. The test asserts exactly one KindCredentialLinked was
//     published in total.
//
// The credential_linked watcher is pre-seeded with the pre-linked
// credential's state (via createUserIdentityWithCred before watcher
// start) so that the revoke + re-link are the only state changes the
// watcher observes, and the "exactly one" count is reliable.
//
// No real names: alice / Triage Bot / Linear / example.com are all
// fictional per AGENTS.md.
package passthrough_revoke_test

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
	// starterEmail is the human the test impersonates. Mapped to a
	// canonical SpiceDB subject via identity.Principal.Canonical().
	starterEmail = "alice@example.com"

	// linearCred is the credential the AgentClass's MCPServer requires
	// via spec.auth.credential.
	linearCred = "linear-oauth"

	// preLinkedToken is the bearer token value seeded before the test
	// starts — the "already linked in a prior session" credential.
	preLinkedToken = "pre-linked-token-7a8b9c0d1e2f"

	// reLinkedToken is the bearer token value the re-link flow submits
	// via POST /link/submit after the revoke.
	reLinkedToken = "re-linked-token-3f4e5d6c7b8a"

	// signingKey is the 32-byte HMAC key shared by the in-test
	// CredentialRequestWatcher (channelsd side, mints the link) and the
	// in-test identityd (verifies the link + sets the cookie). Same key,
	// all three roles — mint, verify, cookie.
	signingKey = "passthrough-revoke-test-key-12345" // 32 bytes
)

// TestPassthroughRevokeReLink is the end-to-end assertion for the
// revoke + re-link cycle (Slice 2.5 phase δ2). One linear test body so
// a failure surfaces the step that broke.
func TestPassthroughRevokeReLink(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	// Split the MCPServer doc out so it can be applied AFTER the harness's
	// MCPStub URL is known. Same pattern as selfservice + portal scenarios.
	rawManifests := string(manifests)
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(rawManifests)

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    starterEmail,
	})

	// Apply the MCPServer now that h.MCP.URL() is known.
	h.ApplyManifest(strings.ReplaceAll(mcpServerDoc, "{{MCP_URL}}", h.MCP.URL()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// envtest doesn't auto-create namespaces; useridentity.PutToken's
	// master-Secret Create lands in agentprimitives-identities.
	createIdentitiesNamespace(t, ctx, h.K8s)

	// The shared HMAC signing key — credential-request watcher mints
	// links with it; identityd verifies links + cookies with it.
	// iss/aud default: matches production channelsd's link-minting role
	// (iss=channelsd, aud=identityd). Cookie Mint calls below explicitly
	// override Issuer=identityd so checkOIDCCookie's
	// WithExpectedIssuer(IssuerIdentityd) gate passes.
	signer := passthroughlink.New([]byte(signingKey),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	// Start in-process identityd on a httptest.Server.
	idBaseURL := startIdentityd(t, h.K8s, signer)
	t.Logf("identityd: %s", idBaseURL)

	// The canonical subject for the test user.
	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err)
	starterCanonical := starterCanon.Subject()
	t.Logf("starterCanonical=%s", starterCanonical)

	// Pre-seed the credential as though the user linked it in a prior
	// session. This creates the master Secret + UserIdentity with one
	// credential entry. We do this BEFORE starting the
	// credential_linked watcher so the watcher primes its snapshot from
	// this pre-existing state, treating the pre-link as baseline.
	// Subsequent changes (revoke → re-link) are then the only delta the
	// watcher observes.
	requirePreLink(t, ctx, h.K8s, starterCanonical, linearCred, preLinkedToken)

	// Task 6 flipped both watchers from a direct SubChannelSenderFor send to
	// a NATS publish (interaction_request / interaction_applied on
	// credential_link) — dial a connection against the harness's embedded
	// NATS server so those publishes reach the harness's already-running
	// outbound relay (see dialHarnessNATS). One shared connection covers
	// both watchers below.
	natsConn := dialHarnessNATS(t, h.NATSURL, "e2e-pt-revoke")

	// Start the credential_linked watcher. On first Run, it primes its
	// in-memory snapshot from the existing UserIdentity (one credential);
	// it will emit nothing for the pre-link. The revoke removes the
	// entry (watcher sees a deletion — no emit, per ι3's design). The
	// re-link adds the credential back (watcher sees an addition → emits
	// exactly one credential_link interaction_applied envelope).
	startCredentialLinkedWatcher(t, h.K8s, 500*time.Millisecond, natsConn.Publish)

	// Wire the CredentialRequestWatcher so it publishes credential_link
	// interaction_request envelopes to the fake channel when new parked
	// sessions appear.
	startCredentialRequestWatcher(t, h.K8s, signer, idBaseURL, natsConn.Publish)

	// Provide a minimal LLM script for after the session un-parks.
	// Without scripted rules ScriptedLLM fatals on the first unmatched
	// request when the operator spawns a runner.
	h.LLM.OnUserMessage("hello").Reply(e2e.RespondToUser("hi"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	// Block until the AgentClass becomes Valid=True.
	h.WaitForAgentClassValid("triage-bot", 30*time.Second)

	// Wait for the fake listener to register its Driver. The listener
	// starter polls Channels every 250ms; by the time AgentClass is
	// Valid the listener has usually started, but poll explicitly to
	// avoid a race with the first SendUserMessage below.
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

	// === Step A: Revoke the pre-linked credential via the portal. ===
	t.Log("Step A: revoke pre-linked credential via portal")

	// Mint an idd_session cookie as identityd's /oidc/callback would.
	// Subject must equal starterCanonical for the cookie gate to pass.
	// checkOIDCCookie verifies iss=identityd aud=identityd after the
	// JWT-claims hardening; override the Signer's default iss=channelsd
	// here so the cookie matches what production setTrustLinkCookie writes.
	cookieRaw, err := signer.Mint(passthroughlink.Payload{
		Issuer:    passthroughlink.IssuerIdentityd,
		Audience:  passthroughlink.AudienceIdentityd,
		Subject:   starterCanonical,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err, "mint idd_session cookie for revoke")
	cookie := &http.Cookie{Name: "idd_session", Value: cookieRaw}

	httpClient := &http.Client{
		// Don't chase redirects — any unexpected 302 surfaces as "unexpected
		// redirect" rather than silently chasing to an error page.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	revokeURL := idBaseURL + "/my/accounts/" + linearCred + "/revoke"
	revokeReq, err := http.NewRequestWithContext(ctx, http.MethodPost, revokeURL, nil)
	require.NoError(t, err, "build POST revoke request")
	revokeReq.AddCookie(cookie)
	revokeResp, err := httpClient.Do(revokeReq)
	require.NoError(t, err, "POST /my/accounts/%s/revoke", linearCred)
	revokeBody := readAndCloseBody(t, revokeResp)
	// Revoke is now Post/Redirect/Get: success → 303 back to the portal with
	// a one-time revoked notice the React portal surfaces as a banner.
	require.Equal(t, http.StatusSeeOther, revokeResp.StatusCode,
		"revoke success → 303 redirect: status=%d body=%s", revokeResp.StatusCode, revokeBody)
	assert.Equal(t, "/my/accounts?notice=revoked:"+linearCred, revokeResp.Header.Get("Location"),
		"revoke must redirect back to the portal with a revoked notice for %q", linearCred)

	// Verify master Secret deleted.
	uiName := useridentity.NameForSubject(starterCanonical)
	masterSecretName := useridentity.MasterSecretName(uiName, linearCred)
	var deletedSec corev1.Secret
	getErr := h.K8s.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      masterSecretName,
	}, &deletedSec)
	require.True(t, apierrors.IsNotFound(getErr),
		"master Secret must be deleted after revoke; get returned: %v", getErr)

	// Verify UserIdentity no longer holds the linear-oauth entry.
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Name: uiName}, &ui),
		"get UserIdentity %s after revoke", uiName)
	for _, cred := range ui.Spec.Credentials {
		assert.NotEqual(t, linearCred, cred.Name,
			"UserIdentity must no longer hold %s after revoke", linearCred)
	}

	// === Step B: New session parks awaiting the now-missing credential. ===
	t.Log("Step B: start new session; expect AwaitingCredentials")

	// SendUserMessage creates an AgentSession via the fake channel's
	// inbound path. The passthrough gate fires on the first reconcile and
	// sees the credential is absent → parks.
	h.SendUserMessage("hello")

	sess := waitForParkedSession(t, ctx, h.K8s, starterCanonical)
	t.Logf("parked session: %s/%s", sess.Namespace, sess.Name)

	// === Step C: Wait for credential_request envelope; drive re-link. ===
	t.Log("Step C: wait for credential_request envelope and drive /link + /link/submit")

	ch := singleChannel(t, ctx, h.K8s)
	drv := fake.DriverFor(ch.Namespace, ch.Name)
	require.NotNil(t, drv, "fake.DriverFor(%s/%s) — listener may not have started",
		ch.Namespace, ch.Name)

	rec := waitForCredentialPrompt(t, ctx, drv, sess.Namespace+"/"+sess.Name)
	assert.Equal(t, sess.Namespace+"/"+sess.Name,
		rec.Payload.AgentSessionRef.Namespace+"/"+rec.Payload.AgentSessionRef.Name,
		"credential_link interaction_request's AgentSessionRef must match the parked session")
	require.NotNil(t, rec.Payload.Audience.Requester,
		"credential_link interaction_request must address a requester")
	reqCanon, canonErr := rec.Payload.Audience.Requester.Principal().AllowSynthetic().Canonical()
	require.NoError(t, canonErr, "credential_link requester canonical")
	assert.Equal(t, starterCanon, reqCanon,
		"credential_link interaction_request's requester must match the user")
	require.Len(t, rec.Payload.Actions, 1,
		"credential_link interaction_request carries exactly one connect action")
	linkAction := rec.Payload.Actions[0]
	assert.True(t, strings.HasPrefix(linkAction.URL, idBaseURL+"/link?d="),
		"credential_link link action URL should start with identityd's /link endpoint: got %q",
		linkAction.URL)

	// Drive the re-link web flow with the same cookie minted for the revoke.
	// The re-link uses GET /link + POST /link/submit — the reactive
	// deep-link path (Slice 2), not the portal path.

	// GET /link — verify the React identity-link app mounts with a bootstrap
	// row for the credential to re-link.
	getLinkReq, err := http.NewRequestWithContext(ctx, http.MethodGet, linkAction.URL, nil)
	require.NoError(t, err, "build GET /link request")
	getLinkReq.AddCookie(cookie)
	getLinkResp, err := httpClient.Do(getLinkReq)
	require.NoError(t, err, "GET /link")
	getLinkBody := readAndCloseBody(t, getLinkResp)
	require.Equal(t, http.StatusOK, getLinkResp.StatusCode,
		"GET /link with valid cookie: status=%d body=%s", getLinkResp.StatusCode, getLinkBody)
	assert.Contains(t, getLinkBody, `data-app="identity-link"`,
		"/link should mount the identity-link React app: body=%s", getLinkBody)
	assert.Contains(t, getLinkBody, `"credentialName":"`+linearCred+`"`,
		"/link bootstrap should carry a row for %q: body=%s", linearCred, getLinkBody)

	// POST /link/submit — re-link with the new token (per-credential).
	parsed, err := url.Parse(linkAction.URL)
	require.NoError(t, err, "parse link action URL")
	rawSignedLink := parsed.Query().Get("d") + "." + parsed.Query().Get("sig")

	form := url.Values{
		"link":       {rawSignedLink},
		"credential": {linearCred},
		"token":      {reLinkedToken},
	}
	submitReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		idBaseURL+"/link/submit", strings.NewReader(form.Encode()))
	require.NoError(t, err, "build POST /link/submit request")
	submitReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	submitReq.AddCookie(cookie)
	// Per-row submit ends in a 302 back to /link?d=&sig= so the user
	// sees the updated row badge. Use a no-redirect client.
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
		"redirect must land back on the menu page so the user sees the updated badge")

	// Verify the master Secret was re-created with the new token.
	var newSec corev1.Secret
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      masterSecretName,
	}, &newSec), "master Secret should exist after re-link")
	assert.Equal(t, []byte(reLinkedToken), newSec.Data["token"],
		"master Secret data[token] should carry the re-linked token")

	// Verify UserIdentity has the credential back.
	var uiAfter spiceboxv1alpha1.UserIdentity
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Name: uiName}, &uiAfter),
		"get UserIdentity %s after re-link", uiName)
	found := false
	for _, c := range uiAfter.Spec.Credentials {
		if c.Name == linearCred {
			found = true
			break
		}
	}
	assert.True(t, found, "UserIdentity should hold %s after re-link", linearCred)

	// === Step D: Session un-parks. ===
	t.Log("Step D: wait for session to un-park (CredentialsReady=True)")
	waitForUnpark(t, ctx, h.K8s, sess.Namespace, sess.Name)

	// === Step E: Exactly one credential_link interaction_applied OOB
	// confirmation. ===
	// The revoke must NOT have emitted (ι3's design: only additions and
	// replacements emit). The re-link MUST emit. So the total count is 1.
	// Task 6 flipped CredentialLinkedWatcher.emit from a direct
	// credential_linked sub-channel send to a NATS-published
	// interaction_applied(category=credential_link, outcome=resolved) —
	// Driver.InteractionApplieds() is where it lands now
	// (Driver.CredentialLinkeds() is dead post-flip).
	t.Log("Step E: assert exactly one credential_link interaction_applied (re-link only)")
	// Give the watcher a moment to observe the re-link beyond the unpark
	// poll. The watcher's PollInterval is 500ms; by the time we reach here
	// the operator has already written the credential (which triggered the
	// unpark), so the watcher's next tick will have fired. Poll briefly to
	// avoid a tight race on the very first tick.
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
		"credential_link interaction_applied should appear within 15s of re-link")

	linked := credentialLinkedApplieds()
	assert.Len(t, linked, 1,
		"exactly one credential_link interaction_applied expected: revoke must not emit, re-link must emit once")
	if len(linked) > 0 {
		assert.Equal(t, channelevents.OutcomeResolved, linked[0].Payload.Outcome,
			"credential_link interaction_applied outcome should be resolved (out-of-band confirmation)")
		assert.Equal(t, linearCred, linked[0].Payload.RequestRef,
			"credential_link interaction_applied should name the re-linked credential via RequestRef")
		assert.Equal(t, sess.Namespace+"/"+sess.Name,
			linked[0].Payload.AgentSessionRef.Namespace+"/"+linked[0].Payload.AgentSessionRef.Name,
			"credential_link interaction_applied should target the re-parked session")
	}
}

// ----- helpers -----------------------------------------------------------

// requirePreLink seeds a pre-linked credential via useridentity.PutToken.
// This simulates a user who linked the credential in a prior session.
// Creates both the master Secret and the UserIdentity entry.
func requirePreLink(t *testing.T, ctx context.Context, c client.Client, subject identity.Subject, credName, token string) {
	t.Helper()
	err := useridentity.PutToken(ctx, c, useridentity.PutTokenRequest{
		Subject:        subject,
		CredentialName: credName,
		Token:          token,
	})
	require.NoError(t, err, "requirePreLink: PutToken for %s/%s", subject, credName)
}

// startIdentityd brings up an in-process identityd HTTP server on a
// httptest.Server. Returns the externally reachable base URL.
//
// Same chicken-and-egg pattern as the passthrough_selfservice and
// passthrough_portal scenarios: bind an unstarted server first (URL
// becomes computable), construct identityd.Server with that URL, then
// start.
// identityd's /link + /portal pages are React apps rendered through the
// webui framework: the page handlers read the framework renderer off the
// request context and emit an empty 200 if it is absent. So the harness
// mounts identityd's routes through a real webui.Server (as internal/cmd/webd does
// in production), not identityd.Server.Handler() directly — only the
// framework path injects the renderer that produces the React document.
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
// False: every /link and /my/accounts request in this scenario carries
// a pre-minted idd_session cookie, so the InsecureTrustLinks path is
// never taken.
func (d harnessWebDeps) InsecureTrustLinks() bool { return false }

// startCredentialLinkedWatcher constructs a pipeline.CredentialLinkedWatcher
// and runs it in a goroutine bound to its own context. PollInterval is
// small (500ms) so the test's Eventually deadline can be tight.
//
// Calling this AFTER requirePreLink ensures the watcher's initial prime
// includes the pre-linked credential as baseline, so the revoke (deletion)
// doesn't emit and only the re-link (addition) emits.
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

// startCredentialRequestWatcher constructs a pipeline.CredentialRequestWatcher
// and runs it in a goroutine. The watcher's initial tick fires at Run()
// start and will catch the parked session as soon as it exists.
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
// embedded NATS server (h.NATSURL). Task 6 flipped both credential watchers
// from a direct SubChannelSenderFor send to a NATS publish of
// interaction_request / interaction_applied (credential_link); this
// connection is what lets those publishes reach the harness's
// already-running outbound relay (subscribed to ap.session.*.*.out.> on its
// own unexported connection — a separate client connection to the same
// embedded server still gets routed). Mirrors the dial pattern in
// test/e2e/scenarios/passthrough_revoke_propagation.
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
// against the channel kind registry. Mirrors the same helper from the
// selfservice and portal scenarios; inlined per-scenario to avoid
// cross-package coupling.
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

// singleChannel returns the single Channel CR in the cluster; mirrors
// the same helper from the portal scenario.
func singleChannel(t *testing.T, ctx context.Context, c client.Client) *spiceboxv1alpha1.Channel {
	t.Helper()
	var channels spiceboxv1alpha1.ChannelList
	require.NoError(t, c.List(ctx, &channels), "list Channels")
	require.Len(t, channels.Items, 1, "scenario assumes one Channel CR")
	return &channels.Items[0]
}

// waitForParkedSession polls until an AgentSession in the default
// namespace reaches AwaitingCredentials for the given starter. Returns
// the session. Mirrors the same helper from passthrough_selfservice.
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
	t.Fatalf("waitForParkedSession: no session reached AwaitingCredentials within 30s; "+
		"last observed: %s/%s phase=%q starter=%q",
		last.Namespace, last.Name, last.Status.Phase,
		last.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID])
	return nil
}

// waitForCredentialPrompt polls the fake Driver's recorded interaction-prompt
// queue until a credential_link interaction_request for the given sessionRef
// is recorded. Post-Task-6-flip counterpart of the old
// credential_request-sub-channel based waiter — mirrors the same helper from
// passthrough_selfservice.
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

// waitForUnpark polls until both CredentialsReady=True on the
// AgentSession AND Ready=True on the SessionUserIdentity.
// Mirrors the same helper from passthrough_selfservice.
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

	// Diagnostic dump on timeout.
	var sess spiceboxv1alpha1.AgentSession
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		t.Fatalf("waitForUnpark: session %s/%s never un-parked AND final Get failed: %v", ns, name, err)
	}
	credReady := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
	var sui spiceboxv1alpha1.SessionUserIdentity
	_ = c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sui)
	suiReady := meta.FindStatusCondition(sui.Status.Conditions, spiceboxv1alpha1.SessionUserIdentityConditionReady)
	t.Fatalf("waitForUnpark: session %s/%s did not un-park within 60s\n"+
		"  session.Phase=%q\n  CredentialsReady=%+v\n"+
		"  SUID.MissingCredentials=%v\n  SUID.Ready=%+v",
		ns, name, sess.Status.Phase, credReady, sui.Status.MissingCredentials, suiReady)
}

// splitMCPServerFromManifests separates the MCPServer YAML document from
// the rest of the multi-doc YAML. Mirrors the same helper from the
// selfservice and portal scenarios.
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

// readAndCloseBody reads + closes the response body. Returns the body as
// a string for easier substring assertions.
func readAndCloseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return string(b)
}
