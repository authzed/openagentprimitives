//go:build e2e

// Package passthrough_replace_test is the end-to-end scenario for the
// user-facing "replace a linked passthrough credential's value" path: a
// session with a credential linked BEFORE it starts (so it reaches
// CredentialsReady=True without ever parking), then a portal replace
// with a NEW token value for the SAME credential, after which the
// operator re-projects the session's per-session credential Secret
// (spiceboxv1alpha1.PassthroughCredentialSecretName) to the new value —
// exactly what the next tool call would resolve.
//
// What this proves end-to-end:
//
//  1. A pre-linked credential (seeded via useridentity.PutToken before the
//     test, simulating "linked in a prior session") lets a fresh
//     userPassthrough AgentSession reach CredentialsReady=True on its
//     first reconcile — no AwaitingCredentials park, unlike the
//     passthrough_revoke scenario which starts from a missing credential.
//     The per-session projected Secret holds the pre-linked value.
//
//  2. identityd's standing-portal replace endpoint — POST
//     /my/accounts/<cred>/link/submit (handlePortalLinkSubmit) — is the
//     SAME useridentity.PutToken-backed mechanism the reactive /link/submit
//     deep-link flow (passthrough_revoke's re-link step) uses, driven here
//     without a signed deep-link: the cookie alone gates it. Submitting a
//     NEW token for the credential the user already has linked overwrites
//     the master Secret's value in place (PutToken's idempotent-replace
//     semantics — see pkg/platform/identity/useridentity/store.go).
//
//  3. The replace re-projects with NO user nudge. Because the token VALUE
//     changed, useridentity.PutToken re-stamps the UserIdentity's
//     rotation-fingerprint annotation, so the (otherwise byte-identical, since
//     the credential Spec references the master Secret by name only) UserIdentity
//     write is a real change. The AgentSession controller's UserIdentity watch
//     (sessionsForUserIdentityChange) re-enqueues the subject's non-terminal
//     sessions, and reconcilePassthroughIdentity re-projects the per-session
//     Secret from the CURRENT master Secret value — with no follow-up message.
//     This is the fix for the same-name/different-value replace gap: without the
//     fingerprint annotation, a value-only rotation is a no-op UserIdentity
//     Update that never fires the watch, so the session would only pick up the
//     new token on some later unrelated reconcile.
//
// The credential-invalidation EMIT itself (the oap.revocation bus envelope
// that drops a live broker's cache entry) is already proven end-to-end by
// pkg/controllers/agentsession's TestPassthroughCredentialInvalidation_Envtest
// (Task 9); this scenario does not re-observe the bus. It proves the
// user-facing surface: portal Replace -> PutToken -> operator re-project of
// the K8s Secret a sandboxed tool call's credential source resolves from.
//
// No real names: alice / Triage Bot / Linear / example.com are all
// fictional per AGENTS.md.
package passthrough_replace_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
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

	// linearCred is the credential the AgentClass's MCPServer requires via
	// spec.auth.credential.
	linearCred = "linear-oauth"

	// tok1 is the value seeded before the session starts — the "already
	// linked" credential that lets the session skip AwaitingCredentials.
	tok1 = "tok-1"

	// tok2 is the value the portal replace submits for the SAME credential.
	tok2 = "tok-2"

	// signingKey is the 32-byte HMAC key the in-test identityd verifies the
	// idd_session cookie with. Only one role needed here (unlike
	// passthrough_revoke, which also mints deep-links) since the portal
	// replace endpoint is cookie-gated only.
	signingKey = "passthrough-replace-test-key-123456" // 32 bytes
)

// TestPassthroughReplace is the end-to-end assertion for the user-facing
// "replace a linked credential" path. One linear test body so a failure
// surfaces the step that broke.
func TestPassthroughReplace(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	// Split the MCPServer doc out so it can be applied AFTER the harness's
	// MCPStub URL is known. Same pattern as the sibling passthrough scenarios.
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(string(manifests))

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

	// The HMAC signing key identityd verifies the idd_session cookie with.
	// iss/aud default: matches production channelsd's link-minting role;
	// the cookie Mint call below explicitly overrides Issuer=identityd so
	// checkOIDCCookie's WithExpectedIssuer(IssuerIdentityd) gate passes.
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

	// === Step 1: pre-link tok-1 BEFORE any session exists. =================
	t.Log("Step 1: pre-link linear-oauth=tok-1 via useridentity.PutToken")
	requirePreLink(t, ctx, h.K8s, starterCanonical, linearCred, tok1)

	uiName := useridentity.NameForSubject(starterCanonical)
	masterSecretName := useridentity.MasterSecretName(uiName, linearCred)

	// Minimal LLM script: reply, then park Idle via await_user_message (the
	// "yield-at-entry" idiom — production's way to end a turn and await the
	// next inbound; EndTurn has no tool_use and loops the runner into a
	// nudge-and-retry cycle until MaxTurnsExceeded, per chat_resume_test.go's
	// established finding). Rule ORDER matters: the tool_result rule is
	// registered FIRST so it wins the match once respond_to_user's result
	// comes back — matchUserText would otherwise walk back past the tool
	// result and keep re-matching the original "hello" text, re-replying
	// forever. Both rules Repeating so a second inbound (Step 3 below) is
	// handled identically.
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("await_user_message", map[string]any{})).Repeating()
	h.LLM.OnUserMessage("hello").Reply(e2e.RespondToUser("hi")).Repeating()

	// Block until the AgentClass becomes Valid=True.
	h.WaitForAgentClassValid("triage-bot", 30*time.Second)

	// Drive the first inbound. SendUserMessage internally polls for the fake
	// driver's registration, so no separate wait is needed here.
	h.SendUserMessage("hello")
	h.ExpectAgentReply(e2e.Contains("hi"))

	// The credential was present before the session was even created, so the
	// passthrough gate must resolve it on the FIRST reconcile — the session
	// must reach CredentialsReady=True WITHOUT ever visiting
	// AwaitingCredentials (contrast with passthrough_revoke, which starts
	// parked).
	sess := waitForCredentialsReadySession(t, ctx, h.K8s, starterCanonical)
	t.Logf("ready session: %s/%s", sess.Namespace, sess.Name)
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, sess.Status.Phase,
		"session must not have parked: the credential was linked before the session was created")

	projectedName := spiceboxv1alpha1.PassthroughCredentialSecretName(sess.Name)
	pollProjectedSecretValue(t, ctx, h.K8s, sess.Namespace, projectedName, linearCred, tok1,
		"per-session Secret must hold the pre-linked tok-1 before any replace")

	// The turn must fully settle to Idle before Step 3's follow-up message —
	// only an Idle (or AwaitingRetry) session gets the wake-requested-at
	// annotation that triggers a fresh operator reconcile (see
	// pkg/channels/channelsd/pipeline.respawnOnWake / annotateWake).
	waitForIdlePhase(t, ctx, h.K8s, sess.Namespace, sess.Name)

	// === Step 2: portal replace — POST /my/accounts/<cred>/link/submit. ====
	t.Log("Step 2: portal replace via POST /my/accounts/linear-oauth/link/submit")

	// Mint an idd_session cookie as identityd's /oidc/callback would.
	// Subject must equal starterCanonical for the cookie gate to pass.
	cookieRaw, err := signer.Mint(passthroughlink.Payload{
		Issuer:    passthroughlink.IssuerIdentityd,
		Audience:  passthroughlink.AudienceIdentityd,
		Subject:   starterCanonical,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err, "mint idd_session cookie for portal replace")
	cookie := &http.Cookie{Name: "idd_session", Value: cookieRaw}

	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	form := url.Values{"token": {tok2}}
	replaceURL := idBaseURL + "/my/accounts/" + linearCred + "/link/submit"
	replaceReq, err := http.NewRequestWithContext(ctx, http.MethodPost, replaceURL, strings.NewReader(form.Encode()))
	require.NoError(t, err, "build POST /my/accounts/%s/link/submit request", linearCred)
	replaceReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replaceReq.AddCookie(cookie)
	replaceResp, err := noRedirect.Do(replaceReq)
	require.NoError(t, err, "POST /my/accounts/%s/link/submit", linearCred)
	replaceBody := readAndCloseBody(t, replaceResp)
	// Success -> 303 back to the portal with a one-time "linked" notice the
	// React portal surfaces as a banner (Post/Redirect/Get).
	require.Equal(t, http.StatusSeeOther, replaceResp.StatusCode,
		"portal replace success -> 303 redirect: status=%d body=%s", replaceResp.StatusCode, replaceBody)
	assert.True(t, strings.HasPrefix(replaceResp.Header.Get("Location"), "/my/accounts?notice="),
		"portal replace must redirect back to the portal with a notice: got %q", replaceResp.Header.Get("Location"))

	// The master Secret is updated synchronously by PutToken — no reconcile
	// needed to observe this half.
	var masterAfterReplace corev1.Secret
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      masterSecretName,
	}, &masterAfterReplace), "get master Secret after portal replace")
	assert.Equal(t, []byte(tok2), masterAfterReplace.Data["token"],
		"master Secret data[token] must hold the replaced tok-2 immediately after the portal POST")

	// === Step 3: the UserIdentity watch re-projects — NO nudge needed. ========
	// Because the token VALUE changed, PutToken re-stamps the UserIdentity's
	// rotation-fingerprint annotation, so the otherwise byte-identical
	// UserIdentity write is a real change. The AgentSession controller's
	// UserIdentity watch (sessionsForUserIdentityChange) re-enqueues the
	// subject's non-terminal sessions, and reconcilePassthroughIdentity
	// re-projects the per-session Secret — with NO follow-up message. This is
	// the fix for the same-name/different-value replace gap: a running session
	// picks up the new token on its next tool call without any user nudge.
	t.Log("Step 3: poll the per-session Secret for the watch-driven re-projected tok-2 (no wake message)")
	pollProjectedSecretValue(t, ctx, h.K8s, sess.Namespace, projectedName, linearCred, tok2,
		"the UserIdentity watch must re-project the per-session Secret to tok-2 with no follow-up message")
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
// Same chicken-and-egg pattern as the sibling passthrough scenarios: bind
// an unstarted server first (URL becomes computable), construct
// identityd.Server with that URL, then start. identityd's /link + /portal
// pages are React apps rendered through the webui framework: the page
// handlers read the framework renderer off the request context and emit an
// empty 200 if it is absent. So the harness mounts identityd's routes
// through a real webui.Server (as internal/cmd/webd does in production), not
// identityd.Server.Handler() directly — only the framework path injects
// the renderer that produces the React document.
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
// passthrough scenarios were written against (no channel authenticator is
// wired in the harness). False: every /my/accounts request in this
// scenario carries a pre-minted idd_session cookie, so the
// InsecureTrustLinks path is never taken.
func (d harnessWebDeps) InsecureTrustLinks() bool { return false }

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

// waitForCredentialsReadySession polls until an AgentSession in the default
// namespace for the given starter reaches CredentialsReady=True. Returns the
// session. Unlike passthrough_revoke's waitForParkedSession (which waits for
// AwaitingCredentials), this scenario's credential is linked before the
// session exists, so the session should reach CredentialsReady without ever
// parking.
func waitForCredentialsReadySession(t *testing.T, ctx context.Context, c client.Client, starterCanonical identity.Subject) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last spiceboxv1alpha1.AgentSession
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := c.List(ctx, &list, client.InNamespace("default")); err != nil {
			t.Logf("waitForCredentialsReadySession: list: %v", err)
			time.Sleep(150 * time.Millisecond)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			if s.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] != starterCanonical.String() {
				continue
			}
			last = *s
			credReady := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
			if credReady != nil && credReady.Status == metav1.ConditionTrue {
				return s
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("waitForCredentialsReadySession: no session reached CredentialsReady within 30s; "+
		"last observed: %s/%s phase=%q starter=%q",
		last.Namespace, last.Name, last.Status.Phase,
		last.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID])
	return nil
}

// waitForIdlePhase polls until the named AgentSession reaches phase=Idle —
// the parked state a channel-attached session settles into between turns,
// and the only state (besides AwaitingRetry) that gets a fresh
// wake-requested-at annotation on the next inbound message (see
// pkg/channels/channelsd/pipeline.respawnOnWake).
func waitForIdlePhase(t *testing.T, ctx context.Context, c client.Client, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		var sess spiceboxv1alpha1.AgentSession
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err == nil {
			last = sess.Status.Phase
			if last == spiceboxv1alpha1.AgentSessionPhaseIdle {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("waitForIdlePhase: session %s/%s did not reach Idle within 30s; last phase=%q", ns, name, last)
}

// pollProjectedSecretValue polls the per-session projected credential Secret
// until data[key] equals want, or fails the test with a diagnostic dump.
// Checks both StringData (the create/update path this repo's
// writeSidecarSecret writes) and Data (what a real apiserver round-trip
// exposes StringData as) — mirrors passthroughSecretValue from
// pkg/controllers/agentsession's invalidation envtest.
func pollProjectedSecretValue(t *testing.T, ctx context.Context, c client.Client, ns, name, key, want, msg string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastVal string
	var lastOK bool
	var lastErr error
	for time.Now().Before(deadline) {
		var sec corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sec); err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		lastErr = nil
		if v, ok := sec.StringData[key]; ok {
			lastVal, lastOK = v, true
		} else if v, ok := sec.Data[key]; ok {
			lastVal, lastOK = string(v), true
		} else {
			lastVal, lastOK = "", false
		}
		if lastOK && lastVal == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("pollProjectedSecretValue: %s/%s data[%q] never became %q within 20s "+
		"(last value=%q present=%v getErr=%v): %s",
		ns, name, key, want, lastVal, lastOK, lastErr, msg)
}

// readAndCloseBody reads + closes the response body. Returns the body as a
// string for easier substring assertions.
func readAndCloseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return string(b)
}

// splitMCPServerFromManifests separates the MCPServer YAML document from the
// rest of the multi-doc YAML. Mirrors the same helper from the sibling
// passthrough scenarios.
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
