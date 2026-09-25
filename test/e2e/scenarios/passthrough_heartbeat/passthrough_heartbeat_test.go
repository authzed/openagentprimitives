//go:build e2e

// Package passthrough_heartbeat_test is the Slice-2.5 phase-δ3 end-to-end
// scenario for the heartbeat endpoint. It drives every moving part of the
//
//	"park session in AwaitingCredentials → POST /heartbeat bumps
//	 SessionUserIdentity.status.lastInteractionAt → stop heartbeating →
//	 LastInteractionAt does NOT advance"
//
// chain in a single in-process process.
//
// What this proves end-to-end:
//
//  1. The AgentSession reconciler parks an identityMode=userPassthrough
//     session in AwaitingCredentials and creates the SessionUserIdentity.
//
//  2. POST /heartbeat?session=<ns>/<name> (cookie-gated) stamps
//     SessionUserIdentity.status.lastInteractionAt to "now" on each call.
//     The assertion verifies the field advances and that the timestamp
//     reflects the most recent heartbeat.
//
//  3. Once heartbeating stops, lastInteractionAt does NOT advance further.
//     This proves the field is heartbeat-driven, not auto-bumped by the
//     operator or any other process.
//
// Design note — wiring-only variant:
//
// The test uses CredentialLinkTimeout = 30m (the default) so the
// operator's passthrough reaper never fires during the test window. This
// is intentional: the operator's deadline math (max(ParkedAt,
// LastInteractionAt) + timeout) is unit-tested in the passthrough_gate
// unit tests; this E2E proves only that POST /heartbeat correctly wires
// through to the SUI status field. The wall-clock expiry path is
// reliable in unit tests but would require a ~35s sleep here just to
// confirm the well-tested deadline math — not worth the CI cycle time.
//
// No real names: alice / Triage Bot / Linear / example.com are all
// fictional per AGENTS.md.
package passthrough_heartbeat_test

import (
	"context"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identityd"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// starterEmail is the human the test impersonates. Mapped to a
	// canonical SpiceDB subject via identity.Principal.Canonical().
	starterEmail = "alice@example.com"

	// signingKey is the 32-byte HMAC key shared by the in-test identityd
	// (verifies cookies + heartbeat gate) and the cookie minted by the
	// test. Same key, both roles — mint + verify.
	signingKey = "passthrough-heartbeat-test-key12" // 32 bytes
)

// TestPassthroughHeartbeatExtends is the end-to-end assertion for the
// heartbeat endpoint (Slice 2.5 phase δ3). One linear test body so a
// failure surfaces the exact step that broke.
//
// Variant: wiring-only (see package doc for rationale).
func TestPassthroughHeartbeatExtends(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	rawManifests := string(manifests)
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(rawManifests)

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    starterEmail,
	})

	// Apply the MCPServer now that h.MCP.URL() is known. The MCP probe
	// hits the in-process MCPStub; no tools are needed for this scenario.
	h.ApplyManifest(strings.ReplaceAll(mcpServerDoc, "{{MCP_URL}}", h.MCP.URL()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// envtest doesn't auto-create namespaces; the identities namespace is
	// needed if any code path touches it, even transitively.
	createIdentitiesNamespace(t, ctx, h.K8s)

	// The shared HMAC signing key — identityd verifies cookies with it;
	// the test mints the cookie with it. Same Signer, both roles.
	// iss/aud default: matches production channelsd's link-minting role.
	// Cookie Mint calls below explicitly override Issuer=identityd so
	// checkOIDCCookie's WithExpectedIssuer(IssuerIdentityd) gate passes.
	signer := passthroughlink.New([]byte(signingKey),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	// Start in-process identityd on a httptest.Server.
	idBaseURL := startIdentityd(t, h.K8s, signer)
	t.Logf("identityd: %s", idBaseURL)

	// Block until the AgentClass becomes Valid=True.
	h.WaitForAgentClassValid("triage-bot", 30*time.Second)

	// Wait for the fake listener to register its Driver before calling
	// SendUserMessage (SendUserMessage fatals if no driver is registered).
	{
		var channels spiceboxv1alpha1.ChannelList
		require.NoError(t, h.K8s.List(ctx, &channels), "list Channels")
		require.Len(t, channels.Items, 1, "scenario assumes one Channel CR")
		ch := &channels.Items[0]
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

	// === Step A: Park the session in AwaitingCredentials. ===
	t.Log("Step A: send inbound to park session in AwaitingCredentials")

	// SendUserMessage creates an AgentSession via the fake channel's
	// inbound path. The passthrough gate fires on the first reconcile;
	// seeing no linear-oauth credential, it parks and creates the SUI.
	// The runner is never spawned (session never unparks), so no LLM
	// scripting is required.
	h.SendUserMessage("hello")

	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err)
	starterCanonical := starterCanon.Subject()
	t.Logf("starterCanonical=%s", starterCanonical)

	sess := waitForParkedSession(t, ctx, h.K8s, starterCanonical)
	t.Logf("parked session: %s/%s", sess.Namespace, sess.Name)
	sessionRef := sess.Namespace + "/" + sess.Name

	// === Step B: Confirm the SUI exists with no LastInteractionAt yet. ===
	t.Log("Step B: confirm SUI is created and LastInteractionAt is nil before any heartbeat")
	var suiBefore spiceboxv1alpha1.SessionUserIdentity
	require.NoError(t, h.K8s.Get(ctx,
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &suiBefore),
		"get SessionUserIdentity %s before heartbeat", sessionRef)
	// LastInteractionAt should be nil before any heartbeat is sent.
	// (The operator sets ParkedAt, not LastInteractionAt.)
	assert.Nil(t, suiBefore.Status.LastInteractionAt,
		"LastInteractionAt must be nil before the first heartbeat")

	// === Step C: Mint the idd_session cookie. ===
	// The cookie gate in handleHeartbeat calls checkOIDCCookie, which
	// verifies the HMAC and extracts the subject. The subject must match
	// the session's AnnotationStartedByCanonicalID.
	// identityd's checkOIDCCookie verifies iss=identityd aud=identityd
	// after the JWT-claims hardening; override the Signer's default
	// iss=channelsd here so the cookie matches what production's
	// setTrustLinkCookie writes.
	cookieRaw, err := signer.Mint(passthroughlink.Payload{
		Issuer:    passthroughlink.IssuerIdentityd,
		Audience:  passthroughlink.AudienceIdentityd,
		Subject:   starterCanonical,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err, "mint idd_session cookie")
	cookie := &http.Cookie{Name: "idd_session", Value: cookieRaw}

	httpClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	heartbeatURL := idBaseURL + "/heartbeat?session=" + url.QueryEscape(sessionRef)

	// === Step D: Send 3 heartbeats, ~500ms apart. ===
	// Each heartbeat should advance LastInteractionAt. We capture the
	// wall-clock time just before the first heartbeat so we can assert
	// the field is After it.
	t.Log("Step D: send 3 heartbeats and assert LastInteractionAt advances")

	beforeFirst := time.Now()

	for i := 0; i < 3; i++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, heartbeatURL, nil)
		require.NoError(t, reqErr, "build POST /heartbeat request (i=%d)", i)
		req.AddCookie(cookie)

		resp, doErr := httpClient.Do(req)
		require.NoError(t, doErr, "POST /heartbeat (i=%d)", i)
		bodyBytes := make([]byte, 0)
		if resp.Body != nil {
			bodyBytes = make([]byte, 512)
			n, _ := resp.Body.Read(bodyBytes)
			resp.Body.Close()
			bodyBytes = bodyBytes[:n]
		}
		require.Equal(t, http.StatusNoContent, resp.StatusCode,
			"heartbeat must return 204 No Content (i=%d): status=%d body=%s",
			i, resp.StatusCode, string(bodyBytes))

		time.Sleep(500 * time.Millisecond)
	}

	// === Step E: Assert LastInteractionAt reflects the most recent heartbeat. ===
	t.Log("Step E: assert LastInteractionAt is recent and after beforeFirst")

	var suiAfter spiceboxv1alpha1.SessionUserIdentity
	require.NoError(t, h.K8s.Get(ctx,
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &suiAfter),
		"get SessionUserIdentity %s after heartbeats", sessionRef)

	require.NotNil(t, suiAfter.Status.LastInteractionAt,
		"LastInteractionAt must be stamped after POST /heartbeat calls")

	// The field should be after the wall-clock before the first heartbeat.
	assert.True(t, suiAfter.Status.LastInteractionAt.Time.After(beforeFirst),
		"LastInteractionAt (%s) should be after beforeFirst (%s)",
		suiAfter.Status.LastInteractionAt.Time, beforeFirst)

	// The field should be recent — within the last 5s from now.
	assert.WithinDuration(t, time.Now(), suiAfter.Status.LastInteractionAt.Time, 5*time.Second,
		"LastInteractionAt should reflect the most recent heartbeat (within 5s of now)")

	// The session must still be parked — heartbeats do not change the phase.
	var sessAfter spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(ctx,
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &sessAfter),
		"re-fetch AgentSession %s after heartbeats", sessionRef)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, sessAfter.Status.Phase,
		"session must still be in AwaitingCredentials (heartbeats do not un-park)")

	// === Step F: Stop heartbeating — assert LastInteractionAt does NOT advance. ===
	t.Log("Step F: stop heartbeating, wait 2s, assert LastInteractionAt does not advance")

	lastSeen := suiAfter.Status.LastInteractionAt.Time
	time.Sleep(2 * time.Second)

	var suiStopped spiceboxv1alpha1.SessionUserIdentity
	require.NoError(t, h.K8s.Get(ctx,
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &suiStopped),
		"get SessionUserIdentity %s after heartbeat pause", sessionRef)

	require.NotNil(t, suiStopped.Status.LastInteractionAt,
		"LastInteractionAt must still be set after pause")
	assert.Equal(t, lastSeen, suiStopped.Status.LastInteractionAt.Time,
		"without heartbeats, LastInteractionAt must not advance (field is heartbeat-driven only)")
}

// ----- helpers -----------------------------------------------------------

// startIdentityd brings up an in-process identityd HTTP server on a
// httptest.Server. Returns the externally reachable base URL.
//
// Same chicken-and-egg pattern as the selfservice, portal, and revoke
// scenarios: bind an unstarted server first (URL becomes computable),
// construct identityd.Server with that URL, then start.
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

// createIdentitiesNamespace creates the agentprimitives-identities
// namespace. envtest doesn't auto-create namespaces; any code path that
// touches it (even transitively) will fail without this.
func createIdentitiesNamespace(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace},
	}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %s namespace: %v", spiceboxv1alpha1.IdentitiesNamespace, err)
	}
}

// waitForParkedSession polls until an AgentSession in the default
// namespace reaches AwaitingCredentials for the given starter canonical.
// Returns the session. Mirrors the same helper from the other passthrough
// scenarios; inlined per-scenario to avoid cross-package coupling.
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

// splitMCPServerFromManifests separates the MCPServer YAML document from
// the rest of the multi-doc YAML. Mirrors the same helper from the
// selfservice, portal, and revoke scenarios.
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
