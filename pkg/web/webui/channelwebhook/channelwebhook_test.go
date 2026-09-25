package channelwebhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	kindregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/channelwebhook"
)

// --- fakeKind: a minimal channelkinds.Kind test double -----------------
//
// Every method but Name and WebhookReceiver returns a zero value; the route
// under test only ever calls those two on a Kind.

type fakeKind struct {
	name     string
	receiver channelkinds.WebhookReceiver // nil ⇒ "not webhook-routable"
}

func (k fakeKind) Name() string                { return k.name }
func (k fakeKind) DefaultSessionScope() string { return "auto" }
func (k fakeKind) Capabilities() []string      { return []string{"text"} }

func (k fakeKind) NewListener(channelkinds.Deps) channelkinds.Listener { return nil }
func (k fakeKind) NewSender(channelkinds.Deps) channelkinds.Sender     { return nil }
func (k fakeKind) SubChannelSender(string, channelkinds.Deps) channelkinds.Sender {
	return nil
}
func (k fakeKind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink { return nil }
func (k fakeKind) SupportsMonitoring() bool                                          { return false }
func (k fakeKind) SupportsLiveViewOffer() bool                                       { return false }
func (k fakeKind) NewMonitoringSender(channelkinds.Deps) channelkinds.MonitoringSender {
	return nil
}
func (k fakeKind) SupportedRoles() []string                              { return spiceboxv1alpha1.AllChannelRoles() }
func (k fakeKind) ValidateSpec(*spiceboxv1alpha1.Channel) error          { return nil }
func (k fakeKind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string   { return nil }
func (k fakeKind) RequiredSecretKeys(*spiceboxv1alpha1.Channel) []string { return nil }
func (k fakeKind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}
func (k fakeKind) RenderMention(externalID string) string                    { return externalID }
func (k fakeKind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }
func (k fakeKind) LookupUser(context.Context, channelkinds.LookupDeps, channelkinds.MentionLookupKind, string) (string, string, error) {
	return "", "", nil
}
func (k fakeKind) MentionToolDescription() string { return "" }
func (k fakeKind) UserAttributable() bool         { return true }
func (k fakeKind) DeliversToHuman() bool          { return true }
func (k fakeKind) AllowsSyntheticIdentity() bool  { return false }
func (k fakeKind) RelayedByChannelsd() bool       { return true }
func (k fakeKind) SpawnsSessionOnInbound() bool   { return true }
func (k fakeKind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return nil
}
func (k fakeKind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return k.receiver }
func (k fakeKind) Wizard() channelkinds.Wizard                                    { return nil }

var _ channelkinds.Kind = fakeKind{}

// --- fakeReceiver: a channelkinds.WebhookReceiver test double ----------

type fakeReceiver struct {
	verifyErr error
	onVerify  func(channelkinds.WebhookRequest) // observes headers/body; also flips verified
	verified  *bool

	translate    *channelkinds.WebhookInbound
	translateErr error
}

func (r *fakeReceiver) Verify(_ context.Context, _ channelkinds.WebhookSecrets, req channelkinds.WebhookRequest) error {
	if r.verified != nil {
		*r.verified = true
	}
	if r.onVerify != nil {
		r.onVerify(req)
	}
	return r.verifyErr
}

func (r *fakeReceiver) Translate(_ context.Context, _ *spiceboxv1alpha1.Channel, _ channelkinds.WebhookRequest) (*channelkinds.WebhookInbound, error) {
	return r.translate, r.translateErr
}

var _ channelkinds.WebhookReceiver = (*fakeReceiver)(nil)

// --- test fixtures -------------------------------------------------------

func testChannel(t *testing.T, ns, name, kind string) *spiceboxv1alpha1.Channel {
	t.Helper()
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           kind,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: name + "-creds"},
		},
	}
}

func testSecret(t *testing.T, ns, name string) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{"webhook-secret": []byte("s3kr1t")},
	}
}

// newFakeClient builds a controller-runtime fake client seeded with objs,
// which may be empty (simulating "the Channel does not exist").
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// countingClient wraps a client.Client and counts Get calls, so a test can
// assert the handler never REACHED a Kubernetes read — a structural proof
// that a fail-closed path short-circuited, rather than a behavioral flag
// that happens to coincide with the right answer.
type countingClient struct {
	client.Client
	gets int
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets++
	return c.Client.Get(ctx, key, obj, opts...)
}

// withKind registers k into the real channelkinds registry for the duration
// of the test, then resets it — this route resolves kinds through that
// process-wide registry (kindregistry.Get), so exercising the real lookup
// (rather than a package-local map) is the point.
func withKind(t *testing.T, k channelkinds.Kind) {
	t.Helper()
	kindregistry.Reset()
	kindregistry.Register(k)
	t.Cleanup(kindregistry.Reset)
}

// newHandler builds a channelwebhook.UI over objs + kind and returns its
// single route's http.Handler, ready for httptest.
func newHandler(t *testing.T, publish func(string, []byte) error, objs ...client.Object) http.Handler {
	t.Helper()
	return newHandlerOverClient(t, newFakeClient(t, objs...), publish)
}

// newHandlerOverClient is newHandler with the k8s client supplied directly,
// so a test can wrap it (see countingClient) to observe whether the handler
// even reached a Kubernetes read.
func newHandlerOverClient(t *testing.T, cli client.Client, publish func(string, []byte) error) http.Handler {
	t.Helper()
	if publish == nil {
		publish = func(string, []byte) error { return nil }
	}
	ui, err := channelwebhook.New(cli, publish)
	require.NoError(t, err)
	routes := ui.Routes(nil)
	require.Len(t, routes, 1, "channelwebhook must publish exactly one route")
	require.Equal(t, "/webhooks/{kind}/{ns}/{channel}", routes[0].Pattern)
	require.Equal(t, webui.OriginTrusted, routes[0].Origin, "the public webhook route must stay on the trusted origin, never the sandbox/artifact one")
	require.Equal(t, webui.AuthHandlerManaged, routes[0].Auth, "the credential is carried in the request; the framework must not impose cookie auth")
	require.Equal(t, []string{http.MethodPost}, routes[0].Methods)
	require.NotNil(t, routes[0].Handler)
	return routes[0].Handler
}

func doPost(t *testing.T, h http.Handler, kind, ns, ch string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+kind+"/"+ns+"/"+ch, bytes.NewReader(body))
	req.SetPathValue("kind", kind)
	req.SetPathValue("ns", ns)
	req.SetPathValue("channel", ch)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- New: constructor fails at startup, never at request time ----------

func TestNew_NilK8sClient_ReturnsError(t *testing.T) {
	ui, err := channelwebhook.New(nil, func(string, []byte) error { return nil })
	assert.Error(t, err)
	assert.Nil(t, ui)
}

func TestNew_NilPublish_ReturnsError(t *testing.T) {
	ui, err := channelwebhook.New(newFakeClient(t), nil)
	assert.Error(t, err)
	assert.Nil(t, ui)
}

func TestNew_BothDepsPresent_Succeeds(t *testing.T) {
	ui, err := channelwebhook.New(newFakeClient(t), func(string, []byte) error { return nil })
	require.NoError(t, err)
	require.NotNil(t, ui)
	assert.Equal(t, "channelwebhook", ui.Name())
}

// --- the status-code contract -------------------------------------------

func TestHandle_StatusCodeContractWithProviderRetries(t *testing.T) {
	cases := []struct {
		name       string
		verifyErr  error
		translate  *channelkinds.WebhookInbound
		publishErr error
		wantStatus int
	}{
		{
			name:       "verified and interesting: 202 Accepted, not retried",
			translate:  &channelkinds.WebhookInbound{ChannelKey: "pr:demo-org/demo-repo#7", MessageText: "opened", AuthzSubject: "service:demo-reviewbot"},
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "verified but uninteresting: 204, not retried",
			translate:  nil,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "bad signature: 401, must never be retried",
			verifyErr:  channelkinds.ErrWebhookUnauthenticated,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "publish to the bus failed: 503, the one case that SHOULD be retried",
			translate:  &channelkinds.WebhookInbound{ChannelKey: "pr:demo-org/demo-repo#7"},
			publishErr: errors.New("nats: no responders"),
			wantStatus: http.StatusServiceUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withKind(t, fakeKind{name: "demo", receiver: &fakeReceiver{verifyErr: tc.verifyErr, translate: tc.translate}})
			ch := testChannel(t, "default", "demo-reviewbot-gh", "demo")
			sec := testSecret(t, "default", "demo-reviewbot-gh-creds")

			publish := func(string, []byte) error { return tc.publishErr }
			h := newHandler(t, publish, ch, sec)

			rec := doPost(t, h, "demo", "default", "demo-reviewbot-gh", []byte(`{}`))
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

func TestHandle_PublishedPayloadCarriesTranslateValuesVerbatim(t *testing.T) {
	inb := &channelkinds.WebhookInbound{
		ChannelKey:   "pr:demo-org/demo-repo#7",
		MessageText:  "PR #7 opened by demo-user",
		AuthzSubject: "service:demo-reviewbot",
		Event:        "pull_request",
	}
	withKind(t, fakeKind{name: "demo", receiver: &fakeReceiver{translate: inb}})
	ch := testChannel(t, "default", "demo-reviewbot-gh", "demo")
	sec := testSecret(t, "default", "demo-reviewbot-gh-creds")

	var gotSubject string
	var gotPayload channelevents.WebhookInboundPayload
	publish := func(subject string, payload []byte) error {
		gotSubject = subject
		return json.Unmarshal(payload, &gotPayload)
	}
	h := newHandler(t, publish, ch, sec)

	postedBody := []byte(`{"action":"opened","number":7}`)
	rec := doPost(t, h, "demo", "default", "demo-reviewbot-gh", postedBody)
	require.Equal(t, http.StatusAccepted, rec.Code)

	assert.Equal(t, channelevents.WebhookInboundSubject, gotSubject)
	assert.Equal(t, "default", gotPayload.ChannelNamespace)
	assert.Equal(t, "demo-reviewbot-gh", gotPayload.ChannelName)
	assert.Equal(t, "demo", gotPayload.ChannelKind)
	assert.Equal(t, inb.ChannelKey, gotPayload.ChannelKey)
	assert.Equal(t, inb.MessageText, gotPayload.MessageText)
	assert.Equal(t, inb.AuthzSubject, gotPayload.AuthzSubject)
	// RawDelivery/DeliveryEvent feed trigger_delivery, which a steelthread
	// capture re-signs and replays at this same route — so the bytes carried
	// here must be the exact bytes posted, not a re-derived or re-marshalled
	// copy, and the event must be what Translate itself read.
	assert.Equal(t, postedBody, gotPayload.RawDelivery, "RawDelivery must be the exact posted bytes, verbatim")
	assert.Equal(t, inb.Event, gotPayload.DeliveryEvent)
}

// TestHandle_VerificationFailure_NeverPublishes pins that a failed
// verification changes nothing about the existing 401/no-retry contract: no
// NATS publish happens at all, so there is no RawDelivery/DeliveryEvent
// leaking out of an unauthenticated delivery either.
func TestHandle_VerificationFailure_NeverPublishes(t *testing.T) {
	withKind(t, fakeKind{name: "demo", receiver: &fakeReceiver{verifyErr: channelkinds.ErrWebhookUnauthenticated}})
	ch := testChannel(t, "default", "demo-reviewbot-gh", "demo")
	sec := testSecret(t, "default", "demo-reviewbot-gh-creds")

	published := false
	publish := func(string, []byte) error {
		published = true
		return nil
	}
	h := newHandler(t, publish, ch, sec)

	rec := doPost(t, h, "demo", "default", "demo-reviewbot-gh", []byte(`{"action":"opened"}`))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, published, "a delivery that fails verification must publish nothing at all")
}

// --- fail-closed invariants: the assertion must match the invariant's kind

func TestHandle_KindWithNilReceiver_Is404AndNeverReachesChannelLookup(t *testing.T) {
	// Fail closed: a kind that IS registered but answers WebhookReceiver with
	// nil must never route. There is no receiver object in this case, so
	// there's nothing whose Translate could even be called — the genuinely
	// discriminating claim is that the handler short-circuits at step 2 and
	// never proceeds to step 3 (resolving the Channel CR). A status-only
	// assertion would pass identically whether the code stopped at step 2 or
	// blundered on and 404'd later for an unrelated reason; wrapping the k8s
	// client to count Get calls turns "never reaches Translate" into a
	// structural fact instead of a flag that happened to stay false because
	// nothing could ever have set it.
	withKind(t, fakeKind{name: "demo", receiver: nil})
	ch := testChannel(t, "default", "demo-reviewbot-gh", "demo")
	cc := &countingClient{Client: newFakeClient(t, ch)}

	h := newHandlerOverClient(t, cc, nil)
	rec := doPost(t, h, "demo", "default", "demo-reviewbot-gh", []byte(`{}`))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, 0, cc.gets, "a kind with no receiver must never reach the Channel lookup, let alone Translate")
}

func TestHandle_UnknownKind_Is404(t *testing.T) {
	kindregistry.Reset()
	t.Cleanup(kindregistry.Reset)
	h := newHandler(t, nil)

	rec := doPost(t, h, "nosuch", "default", "demo-reviewbot-gh", []byte(`{}`))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandle_ChannelKindMismatch_Is404AndVerifyNeverCalled(t *testing.T) {
	// A Channel of kind=slack addressed at /webhooks/demo/... must 404 rather
	// than let the "demo" kind's receiver verify a Secret that belongs to a
	// DIFFERENT kind. The behavioral assertion is the "verified" flag: without
	// it, a bug that skipped the mismatch check but still 404'd later (e.g. a
	// coincidental Secret-read failure) would pass a status-only test for the
	// wrong reason.
	var verified bool
	withKind(t, fakeKind{name: "demo", receiver: &fakeReceiver{verified: &verified}})
	// The Channel genuinely exists, but its spec.kind is "slack" — a
	// different registered... well, unregistered-here kind is fine, the
	// mismatch is checked against the URL's kind, not against whether
	// "slack" itself resolves.
	ch := testChannel(t, "default", "demo-reviewbot-gh", "slack")
	sec := testSecret(t, "default", "demo-reviewbot-gh-creds")

	h := newHandler(t, nil, ch, sec)
	rec := doPost(t, h, "demo", "default", "demo-reviewbot-gh", []byte(`{}`))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.False(t, verified, "one kind's receiver must never verify a Channel belonging to a different kind")
}

func TestHandle_MissingChannel_Is404(t *testing.T) {
	withKind(t, fakeKind{name: "demo", receiver: &fakeReceiver{}})
	h := newHandler(t, nil) // no Channel object seeded

	rec := doPost(t, h, "demo", "default", "no-such-channel", []byte(`{}`))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandle_UnreadableSecret_Is500(t *testing.T) {
	withKind(t, fakeKind{name: "demo", receiver: &fakeReceiver{}})
	ch := testChannel(t, "default", "demo-reviewbot-gh", "demo")
	// No Secret object seeded: ch.Spec.CredentialsRef.SecretName resolves to
	// nothing.
	h := newHandler(t, nil, ch)

	rec := doPost(t, h, "demo", "default", "demo-reviewbot-gh", []byte(`{}`))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestHandle_TranslateError_Is400(t *testing.T) {
	withKind(t, fakeKind{name: "demo", receiver: &fakeReceiver{translateErr: errors.New("malformed event body")}})
	ch := testChannel(t, "default", "demo-reviewbot-gh", "demo")
	sec := testSecret(t, "default", "demo-reviewbot-gh-creds")

	h := newHandler(t, nil, ch, sec)
	rec := doPost(t, h, "demo", "default", "demo-reviewbot-gh", []byte(`{}`))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- body capping ---------------------------------------------------------

// TestHandle_OversizedBody_Is413AndNeverReachesVerify pins the fix for the
// truncate-and-continue defect this test used to enshrine: reading exactly
// maxBodyBytes and proceeding hands a real HMAC receiver a body that no
// longer matches its signature, so an oversized delivery used to fail
// Verify and answer 401 — the one status the whole contract promises is
// never retried. The route now reads one byte past the cap, detects the
// overage, and answers 413 (also never retried, but honestly) BEFORE ever
// calling Verify. The `verified` flag is the discriminating assertion: a
// bug that still called Verify on the truncated body would pass a
// status-only check if it also happened to 413 for some other reason.
func TestHandle_OversizedBody_Is413AndNeverReachesVerify(t *testing.T) {
	var verified bool
	withKind(t, fakeKind{name: "demo", receiver: &fakeReceiver{verified: &verified}})
	ch := testChannel(t, "default", "demo-reviewbot-gh", "demo")
	sec := testSecret(t, "default", "demo-reviewbot-gh-creds")
	h := newHandler(t, nil, ch, sec)

	huge := bytes.Repeat([]byte("a"), 20<<20) // far over maxBodyBytes (5MB)
	rec := doPost(t, h, "demo", "default", "demo-reviewbot-gh", huge)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.False(t, verified, "an oversized delivery must be rejected before Verify ever sees the truncated body")
}

// TestHandle_BodyExactlyAtCap_IsNotRejected proves the boundary is > and not
// >=: a delivery of exactly maxBodyBytes is the largest legitimate body this
// route accepts and must reach Verify normally, not 413.
func TestHandle_BodyExactlyAtCap_IsNotRejected(t *testing.T) {
	var sawLen int
	withKind(t, fakeKind{name: "demo", receiver: &fakeReceiver{
		onVerify:  func(req channelkinds.WebhookRequest) { sawLen = len(req.Body) },
		translate: &channelkinds.WebhookInbound{ChannelKey: "pr:demo-org/demo-repo#7"},
	}})
	ch := testChannel(t, "default", "demo-reviewbot-gh", "demo")
	sec := testSecret(t, "default", "demo-reviewbot-gh-creds")
	h := newHandler(t, nil, ch, sec)

	atCap := bytes.Repeat([]byte("a"), 5<<20) // exactly maxBodyBytes
	rec := doPost(t, h, "demo", "default", "demo-reviewbot-gh", atCap)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, 5<<20, sawLen, "a body exactly at the cap must reach Verify byte-for-byte, not truncated")
}
