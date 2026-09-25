package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// --- fixtures -------------------------------------------------------------

const (
	wcNS         = "builder-b"
	wcSession    = "builder-x"
	wcWorkshop   = wcSession + "-workshop"
	wcWSNS       = "ws-abc123"
	wcSidecar    = "builder-toolbox"
	wcIdentity   = "weather-ai"
	wcCredential = "api_key"
	wcChannel    = "ch-fake"
	wcKind       = "fake"
	wcStarter    = "user:" + "quinn"
	wcExternalID = "U_QUINN"
	wcEmail      = "quinn@example.test"
)

// wcFixtureSession returns a builder AgentSession with a started-by
// canonical annotation and a bound channel — everything ReconcileOne needs
// to resolve a card recipient. The caller can mutate before use.
func wcFixtureSession(mutate ...func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wcSession,
			Namespace: wcNS,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: wcChannel,
				spiceboxv1alpha1.LabelChannelKind: wcKind,
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: wcStarter,
				spiceboxv1alpha1.AnnotationStartedByExternalID:  wcExternalID,
				spiceboxv1alpha1.AnnotationStartedByEmail:       wcEmail,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: wcChannel,
				Kind: wcKind,
				Key:  "dm:" + wcExternalID,
			},
		},
	}
	for _, m := range mutate {
		m(sess)
	}
	return sess
}

// wcFixtureWorkshop returns a Workshop provisioned for wcFixtureSession, with
// one credentialRequests entry (identity=weather-ai, credential=api_key,
// authKind=pat) unless overridden via mutate.
func wcFixtureWorkshop(mutate ...func(*spiceboxv1alpha1.Workshop)) *spiceboxv1alpha1.Workshop {
	ws := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wcWorkshop,
			Namespace: wcNS,
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:        spiceboxv1alpha1.NamespacedRef{Namespace: wcNS, Name: wcSession},
			SidecarToolbox: wcSidecar,
			CredentialRequests: []spiceboxv1alpha1.WorkshopCredentialRequest{
				{Identity: wcIdentity, Credential: wcCredential, AuthKind: "pat"},
			},
		},
		Status: spiceboxv1alpha1.WorkshopStatus{
			Namespace: wcWSNS,
		},
	}
	for _, m := range mutate {
		m(ws)
	}
	return ws
}

// newWorkshopCredentialWatcher wires a WorkshopCredentialWatcher against a
// fake k8s client + a fresh capturingPublisher. now is fixed so minted links
// verify regardless of when the test runs.
func newWorkshopCredentialWatcher(t *testing.T, objs ...client.Object) (*WorkshopCredentialWatcher, client.Client, *capturingPublisher) {
	t.Helper()
	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		Build()
	pub := &capturingPublisher{}
	w := &WorkshopCredentialWatcher{
		K8s:             cli,
		LinkSigner:      newSigner(t),
		ExternalBaseURL: func() string { return "https://identityd.example.test" },
		NATSPublish:     pub.publish,
		Now:             fixedFutureNow,
	}
	return w, cli, pub
}

func getWorkshopFresh(t *testing.T, cli client.Client, ns, name string) *spiceboxv1alpha1.Workshop {
	t.Helper()
	var ws spiceboxv1alpha1.Workshop
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ws))
	return &ws
}

// --- tests ------------------------------------------------------------

// TestWorkshopCredentialWatcher_DeliversAndStamps is the happy path: one
// undelivered credentialRequests entry mints a link, publishes exactly one
// workshop_credential interaction to the builder session, and stamps
// status.credentialRequests[].deliveredAt + noticeRef.
func TestWorkshopCredentialWatcher_DeliversAndStamps(t *testing.T) {
	sess := wcFixtureSession()
	ws := wcFixtureWorkshop()
	w, cli, pub := newWorkshopCredentialWatcher(t, sess, ws)

	err := w.ReconcileOne(context.Background(), ws)
	require.NoError(t, err)

	require.Equal(t, 1, pub.countInteractionRequests(), "exactly one workshop_credential card published")
	req := pub.findInteractionRequest(t)
	assert.Equal(t, categories.WorkshopCredential, req.Category)
	assert.Equal(t, wcNS, req.AgentSessionRef.Namespace)
	assert.Equal(t, wcSession, req.AgentSessionRef.Name)
	require.Len(t, req.Actions, 1)
	assert.Contains(t, req.Actions[0].URL, "/link?d=")
	assert.Equal(t, channelevents.AudienceRequester, req.Audience.Scope)
	require.NotNil(t, req.Audience.Requester)
	assert.Equal(t, wcEmail, string(req.Audience.Requester.Email))

	got := getWorkshopFresh(t, cli, wcNS, wcWorkshop)
	require.Len(t, got.Status.CredentialRequests, 1)
	st := got.Status.CredentialRequests[0]
	assert.Equal(t, wcIdentity, st.Identity)
	assert.Equal(t, wcCredential, st.Credential)
	require.NotNil(t, st.DeliveredAt)
	assert.Equal(t, req.RequestRef, st.NoticeRef)
}

// TestWorkshopCredentialWatcher_DedupSkipsAlreadyDelivered pins the dedup
// contract: a second reconcile of a Workshop whose status row already
// carries a DeliveredAt for the (identity, credential) pair publishes
// nothing.
func TestWorkshopCredentialWatcher_DedupSkipsAlreadyDelivered(t *testing.T) {
	sess := wcFixtureSession()
	delivered := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ws := wcFixtureWorkshop(func(ws *spiceboxv1alpha1.Workshop) {
		ws.Status.CredentialRequests = []spiceboxv1alpha1.WorkshopCredentialRequestStatus{
			{Identity: wcIdentity, Credential: wcCredential, DeliveredAt: &delivered, NoticeRef: "already-sent"},
		}
	})
	w, _, pub := newWorkshopCredentialWatcher(t, sess, ws)

	err := w.ReconcileOne(context.Background(), ws)
	require.NoError(t, err)
	assert.Equal(t, 0, pub.countInteractionRequests(), "already-delivered entry must not re-publish")
}

// TestWorkshopCredentialWatcher_DeliversOAuthMCP proves an oauth-mcp entry is
// delivered exactly like pat/static: the minted link + card carry no
// auth-kind branch at this layer — the type branch lives at identityd's
// authorize/callback flow (Task 3) — so this watcher publishes one card and
// stamps the delivery the same as any other AuthKind.
func TestWorkshopCredentialWatcher_DeliversOAuthMCP(t *testing.T) {
	sess := wcFixtureSession()
	ws := wcFixtureWorkshop(func(ws *spiceboxv1alpha1.Workshop) {
		ws.Spec.CredentialRequests = []spiceboxv1alpha1.WorkshopCredentialRequest{
			{Identity: wcIdentity, Credential: "oauth_token", AuthKind: "oauth-mcp"},
		}
	})
	w, cli, pub := newWorkshopCredentialWatcher(t, sess, ws)

	err := w.ReconcileOne(context.Background(), ws)
	require.NoError(t, err)
	require.Equal(t, 1, pub.countInteractionRequests(), "oauth-mcp entry must publish a card, same as pat/static")
	req := pub.findInteractionRequest(t)
	assert.Equal(t, categories.WorkshopCredential, req.Category)

	got := getWorkshopFresh(t, cli, wcNS, wcWorkshop)
	require.Len(t, got.Status.CredentialRequests, 1, "oauth-mcp entry must be stamped delivered")
	st := got.Status.CredentialRequests[0]
	assert.Equal(t, wcIdentity, st.Identity)
	assert.Equal(t, "oauth_token", st.Credential)
	require.NotNil(t, st.DeliveredAt)
}

// TestWorkshopCredentialWatcher_FailClosed covers the guards that must skip
// + log rather than panic or publish: an empty status.namespace, a missing
// builder AgentSession, and a builder session with no InputChannel.
func TestWorkshopCredentialWatcher_FailClosed(t *testing.T) {
	t.Run("empty status.namespace: no publish, no panic", func(t *testing.T) {
		sess := wcFixtureSession()
		ws := wcFixtureWorkshop(func(ws *spiceboxv1alpha1.Workshop) {
			ws.Status.Namespace = ""
		})
		w, _, pub := newWorkshopCredentialWatcher(t, sess, ws)

		require.NotPanics(t, func() {
			err := w.ReconcileOne(context.Background(), ws)
			require.NoError(t, err)
		})
		assert.Equal(t, 0, pub.countInteractionRequests())
	})

	t.Run("builder AgentSession missing: no publish, no panic", func(t *testing.T) {
		ws := wcFixtureWorkshop()
		// Deliberately do not seed the builder AgentSession object.
		w, _, pub := newWorkshopCredentialWatcher(t, ws)

		require.NotPanics(t, func() {
			err := w.ReconcileOne(context.Background(), ws)
			require.NoError(t, err)
		})
		assert.Equal(t, 0, pub.countInteractionRequests())
	})

	t.Run("builder session has no InputChannel: no publish, no panic", func(t *testing.T) {
		sess := wcFixtureSession(func(s *spiceboxv1alpha1.AgentSession) {
			s.Spec.InputChannel = nil
		})
		ws := wcFixtureWorkshop()
		w, _, pub := newWorkshopCredentialWatcher(t, sess, ws)

		require.NotPanics(t, func() {
			err := w.ReconcileOne(context.Background(), ws)
			require.NoError(t, err)
		})
		assert.Equal(t, 0, pub.countInteractionRequests())
	})

	t.Run("builder session has no started-by canonical: no publish, no panic", func(t *testing.T) {
		sess := wcFixtureSession(func(s *spiceboxv1alpha1.AgentSession) {
			delete(s.Annotations, spiceboxv1alpha1.AnnotationStartedByCanonicalID)
		})
		ws := wcFixtureWorkshop()
		w, _, pub := newWorkshopCredentialWatcher(t, sess, ws)

		require.NotPanics(t, func() {
			err := w.ReconcileOne(context.Background(), ws)
			require.NoError(t, err)
		})
		assert.Equal(t, 0, pub.countInteractionRequests())
	})
}

// TestWorkshopCredentialWatcher_LinkPayloadShape pins the exact
// passthroughlink.Payload the watcher mints: Purpose=workshop_credential,
// SessionRef names the BUILDER session, AgentIdentityRef names the workshop
// namespace + the requested AgentIdentity, and RequiredCredentials carries
// exactly the one credential this entry asked for.
func TestWorkshopCredentialWatcher_LinkPayloadShape(t *testing.T) {
	sess := wcFixtureSession()
	ws := wcFixtureWorkshop()
	w, _, pub := newWorkshopCredentialWatcher(t, sess, ws)

	err := w.ReconcileOne(context.Background(), ws)
	require.NoError(t, err)

	req := pub.findInteractionRequest(t)
	require.Len(t, req.Actions, 1)
	payload := decodeLinkPayload(t, w.LinkSigner, req.Actions[0].URL)
	assert.Equal(t, "workshop_credential", payload.Purpose)
	assert.Equal(t, wcNS+"/"+wcSession, payload.SessionRef)
	assert.Equal(t, wcWSNS+"/"+wcIdentity, payload.AgentIdentityRef)
	require.Len(t, payload.RequiredCredentials, 1)
	assert.Equal(t, wcCredential, payload.RequiredCredentials[0])
}

// TestWorkshopCredentialWatcher_ReconcileAllDeliversAcrossWorkshops proves
// reconcileAll (the Run-driven path) reaches multiple Workshops in one
// pass, each publishing its own card, and skips a Workshop with no
// credentialRequests entirely.
func TestWorkshopCredentialWatcher_ReconcileAllDeliversAcrossWorkshops(t *testing.T) {
	sess := wcFixtureSession()
	ws1 := wcFixtureWorkshop()
	other := wcFixtureWorkshop(func(ws *spiceboxv1alpha1.Workshop) {
		ws.Name = "builder-y-workshop"
		ws.Spec.Session = spiceboxv1alpha1.NamespacedRef{Namespace: wcNS, Name: "builder-y"}
		ws.Spec.CredentialRequests = nil
	})
	sess2 := wcFixtureSession(func(s *spiceboxv1alpha1.AgentSession) {
		s.Name = "builder-y"
	})
	w, _, pub := newWorkshopCredentialWatcher(t, sess, sess2, ws1, other)

	w.reconcileAll(context.Background(), discardLogger(t))
	assert.Equal(t, 1, pub.countInteractionRequests(), "only the workshop carrying credentialRequests publishes")
}
