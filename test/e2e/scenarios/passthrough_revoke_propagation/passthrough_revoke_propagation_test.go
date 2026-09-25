//go:build e2e

// Package passthrough_revoke_propagation_test is the Slice-3 phase-ι2
// end-to-end scenario for the NATS-mediated broker cache invalidation chain.
//
// What this proves end-to-end, beyond the Slice-2.5 δ2 passthrough_revoke
// scenario (which proves "revoke → new session re-parks → re-link → un-park"):
//
//  1. A pre-linked static credential resolved through an inproc.Broker
//     populates the broker's in-memory cache. A second Resolve serves the
//     cached token without re-reading the master Secret — verified by
//     mutating the Secret between calls and observing the original token.
//
//  2. Revoking the credential through identityd's POST
//     /my/accounts/<cred>/revoke (the same HTTP path δ2 drives) deletes
//     the master Secret and removes the entry from UserIdentity.Spec.
//     A test-local goroutine observes the UserIdentity Spec delta and
//     invokes useridentity.RevokePublisher.Observe, mirroring the
//     production operator's UserIdentity controller — and the publisher's
//     revocation.Publisher publishes a KindRevoked envelope (kind=
//     "credential") on the unified subject ap.revocation over the
//     harness's real NATS server.
//
//  3. The runner's unified revocation subscriber, registered via
//     revocation.RegisterSubscriber against the same NATS server with a
//     registry holding the credential Invalidator, JSON-decodes the
//     envelope, scope-filters it, and dispatches to the credential
//     Invalidator, which parses key "<ns>/<masterSecret>" and calls
//     broker.InvalidateSecret, dropping the cache entry.
//
//  4. A second Resolve issued after the invalidation now misses the cache
//     and attempts a fresh read of the (now-deleted) master Secret,
//     returning a NotFound-wrapped error.
//
// The integration test (pkg/controllers/useridentity/revoke_integration_test.go)
// already proves the publisher→NATS→subscriber→InvalidateSecret chain in
// isolation against an embedded NATS server. This scenario promotes that to a
// real harness integration with a real identityd HTTP revoke path driving the
// UserIdentity change.
//
// No real names: alice / Linear / example.com are all fictional per
// AGENTS.md.
package passthrough_revoke_propagation_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/credential"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	uictrl "github.com/authzed/openagentprimitives/pkg/controllers/useridentity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/platform/identityd"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// starterEmail is the human the test impersonates. Canonicalized into a
	// SpiceDB subject via identity.Principal.Canonical().
	starterEmail = "alice@example.com"

	// linearCred is the credential name we pre-link and then revoke. The
	// broker keys its cache by (NameForSubject(subject), credName) →
	// master-Secret name, so this name is part of the invalidation contract.
	linearCred = "linear-pat"

	// preLinkedToken is the bearer token seeded before the test starts.
	// Verifies cache-hit semantics: a second Resolve after a Secret mutation
	// returns this original value (from cache), not the mutated one.
	preLinkedToken = "pre-linked-token-7a8b9c0d1e2f"

	// mutatedToken is what the test writes into the master Secret AFTER
	// the cache is populated and BEFORE the revoke. It's the value a
	// second Resolve would return iff the broker re-read the Secret — i.e.
	// iff the cache was evicted. Since revoke also deletes the Secret, in
	// this scenario the second Resolve actually errors out (NotFound). The
	// mutatedToken intermediate state exists only to validate the cache
	// works before revocation.
	mutatedToken = "mutated-token-1a2b3c4d5e6f"

	// signingKey is the HMAC key identityd uses for the idd_session cookie
	// (the test mints one to bypass the OIDC bootstrap path, mirroring the
	// δ2 scenario).
	signingKey = "passthrough-revoke-prop-test-key-128" // 32 bytes
)

// TestPassthroughRevokePropagation is the single linear test body; one
// failure surfaces the step that broke.
func TestPassthroughRevokePropagation(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    starterEmail,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// envtest doesn't auto-create namespaces; useridentity.PutToken's
	// master-Secret Create lands in agentprimitives-identities.
	createIdentitiesNamespace(t, ctx, h.K8s)

	// Mint the shared HMAC signer used for identityd's cookie path.
	// iss/aud default: matches production channelsd's link-minting role.
	// The cookie Mint call below explicitly overrides Issuer=identityd
	// so checkOIDCCookie's WithExpectedIssuer(IssuerIdentityd) gate passes.
	signer := passthroughlink.New([]byte(signingKey),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	// Start an in-process identityd backed by the harness's K8s client.
	// The test only drives POST /my/accounts/<cred>/revoke; the auth-bootstrap
	// path is bypassed by minting an idd_session cookie directly below.
	idBaseURL := startIdentityd(t, h.K8s, signer)
	t.Logf("identityd: %s", idBaseURL)

	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err)
	starterCanonical := starterCanon.Subject()
	t.Logf("starterCanonical=%s", starterCanonical)

	// === Step A: Pre-link the credential. ===
	t.Log("Step A: pre-link static credential via useridentity.PutToken")
	require.NoError(t, useridentity.PutToken(ctx, h.K8s, useridentity.PutTokenRequest{
		Subject:        starterCanonical,
		CredentialName: linearCred,
		Token:          preLinkedToken,
	}), "PutToken seed")

	uiName := useridentity.NameForSubject(starterCanonical)
	masterSecretName := useridentity.MasterSecretName(uiName, linearCred)

	// === Step B: Wire a real NATS publisher (operator side) + subscriber
	// (runner-side broker) against the harness's embedded NATS server. ===
	t.Log("Step B: dial NATS publisher + subscriber against harness NATS")

	pubConn, err := nats.Connect(h.NATSURL,
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1),
		nats.Name("e2e-pt-revoke-prop-pub"))
	require.NoError(t, err, "dial publisher NATS conn")
	t.Cleanup(func() {
		if err := pubConn.Drain(); err != nil {
			t.Logf("publisher conn drain on cleanup: %v", err)
		}
	})

	subConn, err := nats.Connect(h.NATSURL,
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1),
		nats.Name("e2e-pt-revoke-prop-sub"))
	require.NoError(t, err, "dial subscriber NATS conn")
	t.Cleanup(func() {
		if err := subConn.Drain(); err != nil {
			t.Logf("subscriber conn drain on cleanup: %v", err)
		}
	})

	// Construct the operator-side RevokePublisher with a NATS-backed
	// revocation.Publisher (the same shape internal/cmd/operator/main.go uses).
	rp := uictrl.NewRevokePublisher(revocation.NewPublisher(&natsEnvelopePublisher{nc: pubConn}))

	// Construct the runner-side broker + unified revocation subscriber. The
	// broker's cache is the system under test; the subscriber dispatches
	// KindRevoked envelopes (kind="credential") to the credential
	// Invalidator, which calls broker.InvalidateSecret.
	//
	// We also wrap the broker to count InvalidateSecret calls so we can assert
	// the subscriber dispatched at least one invalidation in addition to
	// the black-box "second Resolve errors" assertion below.
	rawBroker := inproc.New(h.K8s)
	countingBroker := &countingInvalidator{wrapped: rawBroker}
	reg := revocation.NewRegistry()
	require.NoError(t, reg.Register(credential.New(countingBroker)),
		"register credential invalidator")
	// The runner subscribes with its SESSION namespace, deliberately DISTINCT
	// from IdentitiesNamespace — a userPassthrough session almost never runs in
	// IdentitiesNamespace in production. This proves the cluster-wide credential
	// revoke (scope "") reaches a cross-namespace session; registering with
	// IdentitiesNamespace (the old value) masked the scope-gate regression.
	const sessionNamespace = "team-a"
	require.NoError(t,
		revocation.RegisterSubscriber(ctx,
			&natsSubscriberAdapter{conn: subConn},
			reg,
			sessionNamespace,
		),
		"RegisterSubscriber")
	// Force the subscription to flush to the server before we publish.
	require.NoError(t, subConn.Flush(), "flush subscriber connection")

	// === Step C: Prime the publisher with the pre-link state. ===
	// First Observe() primes without emitting; per RevokePublisher's
	// anti-spam-on-restart rule. This mirrors what the production
	// UserIdentity controller would do on its first reconcile after the
	// PutToken above.
	t.Log("Step C: prime RevokePublisher with pre-link state")
	{
		var ui spiceboxv1alpha1.UserIdentity
		require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Name: uiName}, &ui),
			"get UserIdentity after pre-link")
		rp.Observe(ctx, &ui)
	}

	// === Step D: First Resolve — populates the broker cache. ===
	t.Log("Step D: Resolve credential → broker cache populated")
	desc := spiceboxv1alpha1.CredentialDescriptor{
		Source: spiceboxv1alpha1.CredentialSource{
			Type:      "static",
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
			Name:      masterSecretName,
			Key:       "token",
		},
		Inject: spiceboxv1alpha1.CredentialInjection{EnvVar: "LINEAR_TOKEN"},
	}
	req := broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc}}

	res1, err := rawBroker.Resolve(ctx, req)
	require.NoError(t, err, "first Resolve must succeed")
	assert.Equal(t, preLinkedToken, res1.EnvVars["LINEAR_TOKEN"],
		"broker returns the pre-linked token on the cache-miss path")

	// === Step E: Confirm the cache is actually serving by mutating the
	// Secret and seeing the second Resolve return the original token. ===
	t.Log("Step E: mutate master Secret in-place; assert cache still serves the pre-link token")
	{
		var sec corev1.Secret
		require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
			Name:      masterSecretName,
		}, &sec), "get master Secret before mutation")
		sec.Data["token"] = []byte(mutatedToken)
		require.NoError(t, h.K8s.Update(ctx, &sec), "mutate master Secret")
	}
	res2, err := rawBroker.Resolve(ctx, req)
	require.NoError(t, err, "second Resolve (still cached) must succeed")
	assert.Equal(t, preLinkedToken, res2.EnvVars["LINEAR_TOKEN"],
		"cache hit: second Resolve must serve the cached pre-link token, not the mutated one")

	// === Step F: Revoke via identityd. ===
	t.Log("Step F: POST /my/accounts/<cred>/revoke")
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
	cookie := &http.Cookie{Name: "idd_session", Value: cookieRaw}

	httpClient := &http.Client{
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

	// Sanity: master Secret deleted, UserIdentity entry removed.
	{
		var sec corev1.Secret
		getErr := h.K8s.Get(ctx, client.ObjectKey{
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
			Name:      masterSecretName,
		}, &sec)
		require.True(t, apierrors.IsNotFound(getErr),
			"master Secret must be NotFound after revoke; got err=%v", getErr)
	}
	{
		var ui spiceboxv1alpha1.UserIdentity
		require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Name: uiName}, &ui),
			"get UserIdentity after revoke")
		for _, c := range ui.Spec.Credentials {
			assert.NotEqual(t, linearCred, c.Name,
				"UserIdentity must no longer hold %s after revoke", linearCred)
		}
	}

	// === Step G: Drive the operator-side publisher to detect the spec
	// delta. Mirrors what the UserIdentity controller's Reconcile would do
	// after the spec.Credentials update committed by the revoke handler. ===
	t.Log("Step G: drive RevokePublisher.Observe on the post-revoke UserIdentity")
	{
		var ui spiceboxv1alpha1.UserIdentity
		require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Name: uiName}, &ui),
			"get UserIdentity post-revoke")
		rp.Observe(ctx, &ui)
	}

	// === Step H: Within ~5s, the NATS-delivered envelope reaches the
	// broker's subscriber and Invalidate fires. A second Resolve then
	// misses the cache and reads the (deleted) Secret, returning an
	// error. ===
	t.Log("Step H: assert cache invalidation via second Resolve returning an error")
	require.Eventually(t, func() bool {
		_, err := rawBroker.Resolve(ctx, req)
		return err != nil
	}, 5*time.Second, 100*time.Millisecond,
		"broker cache must be invalidated within 5s of revoke; "+
			"Resolve should miss → read deleted Secret → return error")

	// Belt-and-braces: the subscriber must have actually fired
	// InvalidateSecret for the right master-Secret coords. This guards
	// against a passing "Resolve errored" assertion that's caused by
	// something other than the NATS-driven invalidation (e.g., a cache
	// eviction the broker started doing under load).
	require.Eventually(t, func() bool {
		return countingBroker.invalidatedFor(spiceboxv1alpha1.IdentitiesNamespace, masterSecretName)
	}, 5*time.Second, 50*time.Millisecond,
		"subscriber must have called InvalidateSecret(%q, %q) within 5s of revoke",
		spiceboxv1alpha1.IdentitiesNamespace, masterSecretName)
}

// ----- helpers -----------------------------------------------------------

// natsEnvelopePublisher is a test-local implementation of
// revocation.EventPublisher backed by a *nats.Conn. JSON-encodes the envelope
// and publishes it on the unified revocation.Subject — exactly the same
// shape as internal/cmd/operator/main.go's natsEnvelopePublisher, mirrored here to
// avoid importing main.
type natsEnvelopePublisher struct{ nc *nats.Conn }

func (p *natsEnvelopePublisher) Publish(_ context.Context, env channelevents.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return p.nc.Publish(revocation.Subject, data)
}

// natsSubscriberAdapter adapts a *nats.Conn to revocation.NATSSubscriber.
// Mirrors internal/cmd/runner/nats.go's natsConnAdapter — inlined here to avoid
// importing main.
type natsSubscriberAdapter struct{ conn *nats.Conn }

func (a *natsSubscriberAdapter) Subscribe(subject string, handler func([]byte)) error {
	_, err := a.conn.Subscribe(subject, func(msg *nats.Msg) { handler(msg.Data) })
	return err
}

// countingInvalidator wraps a real broker (inproc.Broker) and counts every
// InvalidateSecret call by (namespace, name) tuple. It satisfies
// credential.SecretInvalidator. The wrapped Resolve is NOT exposed because
// the subscriber only needs the InvalidateSecret slice; the test calls
// Resolve on the raw broker directly to avoid a typed-nil pitfall (the
// broker.Broker contract is wider than SecretInvalidator).
type countingInvalidator struct {
	wrapped *inproc.Broker

	mu    sync.Mutex
	calls map[string]int // "<namespace>|<name>" → count
}

func (c *countingInvalidator) InvalidateSecret(namespace, name string) error {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[namespace+"|"+name]++
	c.mu.Unlock()
	return c.wrapped.InvalidateSecret(namespace, name)
}

func (c *countingInvalidator) invalidatedFor(namespace, name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[namespace+"|"+name] > 0
}

// startIdentityd brings up an in-process identityd HTTP server on a
// httptest.Server. Mirrors the same chicken-and-egg pattern used by the
// selfservice / portal / revoke scenarios: bind an unstarted server first
// (URL becomes computable), construct identityd.Server with that URL,
// then start.
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

// readAndCloseBody reads + closes the response body. Returns the body as
// a string for easier substring assertions.
func readAndCloseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return string(b)
}
