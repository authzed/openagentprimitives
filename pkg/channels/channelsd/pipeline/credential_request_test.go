package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/explainer"
	fakeexplainer "github.com/authzed/openagentprimitives/pkg/channels/channelsd/explainer/fake"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// --- recording test doubles ---------------------------------------------

type recordedSend struct {
	sess channelkinds.SessionInfo
	env  channelevents.Envelope
}

// recordingSender captures every Send call into an in-memory slice.
type recordingSender struct {
	sends []recordedSend
}

func (r *recordingSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	r.sends = append(r.sends, recordedSend{sess: sess, env: env})
	return channelkinds.SubChannelSendResult{}, nil
}

// sendCount reports how many times Send was called — the recorder used
// by the ForcePublish tests to assert dedup-bypass / no-op behavior
// without inspecting envelope contents.
func (r *recordingSender) sendCount() int { return len(r.sends) }

// stubResolver returns a preconfigured sender for the named sub-channel,
// records every lookup, and lets the test simulate "kind doesn't
// implement this sub-channel" by setting Sender to nil.
type stubResolver struct {
	Sender    channelkinds.Sender
	Err       error
	LastName  string
	LastSess  string
	CallCount int
}

func (s *stubResolver) SubChannelSenderFor(_ context.Context, sess *spiceboxv1alpha1.AgentSession, name string) (channelkinds.Sender, error) {
	s.CallCount++
	s.LastName = name
	if sess != nil {
		s.LastSess = sess.Namespace + "/" + sess.Name
	}
	return s.Sender, s.Err
}

// --- fixtures -----------------------------------------------------------

const (
	testNS                = "default"
	testSession           = "session-aaaa1111"
	testClass             = "engineer-bot"
	testStarter           = "user:" + "alice"
	testStarterExternalID = "U_ALICE"
	testStarterEmail      = "alice@example.com"
	testChannelName       = "ch-fake"
	testKindName          = "fake"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "add scheme")
	// corev1 too: the credential-update watcher reads a monitoring Channel's
	// credentials Secret to decide whether that Channel can actually deliver
	// (monitoringChannelUndeliverable), so a fake client without Secrets
	// registered cannot host those cases.
	require.NoError(t, corev1.AddToScheme(scheme), "add corev1 scheme")
	return scheme
}

// fixtureSession returns an AgentSession parked in AwaitingCredentials
// with the started-by canonical annotation set + a channel binding. The
// caller can mutate fields before WithObjects.
func fixtureSession(t *testing.T, mutate ...func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testSession,
			Namespace: testNS,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: testChannelName,
				spiceboxv1alpha1.LabelChannelKind: testKindName,
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: testStarter,
				spiceboxv1alpha1.AnnotationStartedByExternalID:  testStarterExternalID,
				spiceboxv1alpha1.AnnotationStartedByEmail:       testStarterEmail,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: testClass,
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: testChannelName,
				Kind: testKindName,
				Key:  "dm:U_ALICE",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		},
	}
	for _, m := range mutate {
		m(sess)
	}
	return sess
}

// fixtureSUI returns a SessionUserIdentity matching the fixtureSession
// by name. missing + what are independent so a test can construct an
// "explanation mismatches missingCredentials" case if needed.
//
// what is the per-credential Title list; one Explanation Item is built per
// what entry, with Credential taken from the same-index missing entry (or
// the title itself when missing is shorter). whyText is the declared reason
// applied to every item's Why; an EMPTY whyText leaves Why unset — the
// render-time fallback signal the watcher fills via LLM then static.
func fixtureSUI(missing []string, what []string, whyText string) *spiceboxv1alpha1.SessionUserIdentity {
	sui := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: testSession, Namespace: testNS},
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			AgentSession: testSession,
			Subject:      testStarter,
		},
		Status: spiceboxv1alpha1.SessionUserIdentityStatus{
			MissingCredentials: missing,
		},
	}
	if len(what) > 0 {
		items := make([]spiceboxv1alpha1.CredentialExplanationItem, len(what))
		for i, title := range what {
			cred := title
			if i < len(missing) {
				cred = missing[i]
			}
			items[i] = spiceboxv1alpha1.CredentialExplanationItem{
				Credential: cred,
				Title:      title,
				Why:        whyText, // "" ⇒ render-time fallback signal
			}
		}
		sui.Status.Explanation = &spiceboxv1alpha1.CredentialExplanation{Items: items}
	}
	return sui
}

// staticGapWhy is the agent-neutral sentence the watcher fills in for any
// credential whose Why is empty when no AgentClass is wired (acFound=false).
// Mirrors agentClassDisplayName(nil, false) == "the agent".
const staticGapWhy = "the agent needs to call this service as you to complete the work you asked for."

// newSigner returns a deterministic-key signer for tests.
func newSigner(t *testing.T) *passthroughlink.Signer {
	t.Helper()
	return passthroughlink.New([]byte("test-test-test-test-test-test-32")) // 32 bytes
}

// newTestWatcher wires a CredentialRequestWatcher against a fake k8s client +
// a fresh capturingPublisher standing in for NATSPublish, the watcher's only
// delivery path (interaction_request on .out). now is fixed. Returns the
// publisher so tests can decode what was published.
func newTestWatcher(t *testing.T, objs ...client.Object) (*CredentialRequestWatcher, client.Client, *capturingPublisher) {
	t.Helper()
	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SessionUserIdentity{}).
		Build()
	pub := &capturingPublisher{}
	w := &CredentialRequestWatcher{
		K8s:             cli,
		LinkSigner:      newSigner(t),
		ExternalBaseURL: func() string { return "https://identityd.example.test" },
		NATSPublish:     pub.publish,
		// Use a fixed point in the future so passthroughlink.Verify
		// (which compares ExpiresAt against the real wall clock) keeps
		// the link valid throughout the test run regardless of when
		// CI runs.
		Now: fixedFutureNow,
	}
	return w, cli, pub
}

// fixedFutureNow returns a clock value sufficiently in the future
// that w.Now + linkTimeout > realtime, so Verify accepts the link.
// Using a fixed value (rather than time.Now()) keeps assertions on
// ExpiresAt deterministic.
func fixedFutureNow() time.Time {
	return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
}

// decodeLinkPayload parses an emitted LinkURL back into a
// passthroughlink.Payload by re-running the watcher's signer's Verify
// against the URL's ?d= and ?sig= halves. Returns the decoded payload.
func decodeLinkPayload(t *testing.T, signer *passthroughlink.Signer, linkURL string) passthroughlink.Payload {
	t.Helper()
	u, err := url.Parse(linkURL)
	require.NoError(t, err, "parse linkURL")
	d := u.Query().Get("d")
	sig := u.Query().Get("sig")
	require.NotEmpty(t, d, "?d= present")
	require.NotEmpty(t, sig, "?sig= present")
	p, err := signer.Verify(d + "." + sig)
	require.NoError(t, err, "Verify link")
	return p
}

// --- happy-path tests ---------------------------------------------------

func TestCredentialRequestWatcher_HappyPath(t *testing.T) {
	sess := fixtureSession(t)
	declaredWhy := "To open the PR you asked about, the agent needs to act on your behalf in these services."
	sui := fixtureSUI(
		[]string{"GitHub", "Linear"},
		[]string{"GitHub", "Linear"},
		declaredWhy,
	)
	// Seed a non-empty Description on the first item to assert propagation
	// from SUI CredentialExplanationItem.Description → payload Field.Value.
	const githubDescription = "a token to read your repos"
	sui.Status.Explanation.Items[0].Description = githubDescription

	w, cli, pub := newTestWatcher(t, sess, sui)

	require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")

	require.Equal(t, 1, pub.countInteractionRequests(), "exactly one interaction_request published")
	pl := pub.findInteractionRequest(t)

	assert.Equal(t, testNS, pl.AgentSessionRef.Namespace, "AgentSessionRef.Namespace")
	assert.Equal(t, testSession, pl.AgentSessionRef.Name, "AgentSessionRef.Name")
	assert.Equal(t, categories.CredentialLink, pl.Category, "Category")
	assert.NotEmpty(t, pl.RequestRef, "RequestRef is minted")
	assert.Equal(t, "Connect your accounts", pl.Lead, "Lead")

	// *** CRITICAL CONTRACT ***: Audience.Requester carries the raw+email
	// started-by identity (AnnotationStartedByExternalID +
	// AnnotationStartedByEmail), addressed with the session's OWN channel kind —
	// not a hardcoded "slack" — and honestly raw: never a precomputed canonical
	// stuffed into ExternalID. The consumer
	// (pkg/channels/channelkinds/slack/interaction.go's sendRequest) derives the
	// canonical itself via Principal().AllowSynthetic().Canonical() before
	// calling resolveSlackUserIDFromCanonical. See
	// TestCredentialRequestWatcher_HappyPath_NoStartedByEmail_FallsBackToCanonicalSubject
	// below for the no-email fallback.
	require.Equal(t, channelevents.AudienceRequester, pl.Audience.Scope, "Audience.Scope")
	require.NotNil(t, pl.Audience.Requester, "Audience.Requester")
	assert.Equal(t, testKindName, pl.Audience.Requester.Kind.String(), "Audience.Requester.Kind = session's channel kind")
	assert.Equal(t, testStarterExternalID, pl.Audience.Requester.ExternalID.String(), "Audience.Requester.ExternalID = raw started-by external id")
	assert.Equal(t, testStarterEmail, pl.Audience.Requester.Email.String(), "Audience.Requester.Email = verified started-by email")
	assert.Empty(t, pl.Audience.Requester.Subject, "Subject unset: email is present, no synthetic-canonical fallback needed")

	// Fields is assembled from SUI Explanation: Label from item Title, Value
	// composes Description + Why (declared reason verbatim here).
	require.Len(t, pl.Fields, 2, "Fields has one entry per SUI explanation item")
	assert.Equal(t, "GitHub", pl.Fields[0].Label, "Fields[0].Label from Item.Title")
	assert.Equal(t, "Linear", pl.Fields[1].Label, "Fields[1].Label from Item.Title")
	assert.Equal(t, githubDescription+" — "+declaredWhy, pl.Fields[0].Value,
		"Fields[0].Value = Description + declared Why")
	assert.Equal(t, declaredWhy, pl.Fields[1].Value,
		"Fields[1].Value = declared Why verbatim (no Description to prepend)")

	// Exactly one link action carries the minted URL.
	require.Len(t, pl.Actions, 1, "exactly one action")
	action := pl.Actions[0]
	assert.Equal(t, channelevents.ActionKindLink, action.Kind, "action.Kind")
	require.True(t, strings.HasPrefix(action.URL, "https://identityd.example.test/link?"),
		"action.URL points at externalBaseURL: %s", action.URL)

	// The link URL verifies + carries the SUI's missingCredentials verbatim.
	decoded := decodeLinkPayload(t, w.LinkSigner, action.URL)
	assert.Equal(t, testNS+"/"+testSession, decoded.SessionRef, "payload SessionRef")
	assert.Equal(t, identity.Subject(testStarter), decoded.Subject, "payload Subject")
	assert.Equal(t, []string{"GitHub", "Linear"}, decoded.RequiredCredentials, "payload RequiredCredentials verbatim")
	// Default 30-minute window when no AgentClass override.
	expected := w.Now().Add(DefaultCredentialLinkTimeout).Unix()
	assert.Equal(t, expected, decoded.ExpiresAt, "ExpiresAt = now + 30m")

	// Condition stamped True for dedup — markPublished is unchanged by the flip.
	var refetched spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &refetched), "Get session")
	assert.True(t, alreadyPublished(&refetched), "CredentialRequestPublished condition is True")

	// Second reconcile is a no-op (dedup).
	require.NoError(t, w.ReconcileOne(context.Background(), &refetched), "second ReconcileOne")
	require.Equal(t, 1, pub.countInteractionRequests(), "NOT published again after dedup")
}

// TestCredentialRequestWatcher_HappyPath_NoStartedByEmail_FallsBackToCanonicalSubject
// covers a started-by user with no verified email on record (e.g. a session
// predating the ExternalID/Email annotation pair, or a channel-native id the
// channel kind never resolved an email for): doPublish cannot safely
// re-derive the synthetic kind:teamScope:externalID canonical (there is no
// StartedByTeamScope annotation preserving the TeamScope the original
// canonical was minted with), so it falls back to the Subject passthrough —
// the exact precomputed canonical (AnnotationStartedByCanonicalID) — keeping
// delivery byte-identical instead of risking a divergent re-derivation.
func TestCredentialRequestWatcher_HappyPath_NoStartedByEmail_FallsBackToCanonicalSubject(t *testing.T) {
	sess := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
		delete(s.Annotations, spiceboxv1alpha1.AnnotationStartedByExternalID)
		delete(s.Annotations, spiceboxv1alpha1.AnnotationStartedByEmail)
	})
	sui := fixtureSUI([]string{"GitHub"}, []string{"GitHub"}, "why")

	w, _, pub := newTestWatcher(t, sess, sui)
	require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")

	require.Equal(t, 1, pub.countInteractionRequests(), "exactly one interaction_request published")
	pl := pub.findInteractionRequest(t)

	require.NotNil(t, pl.Audience.Requester, "Audience.Requester")
	assert.Equal(t, testKindName, pl.Audience.Requester.Kind.String(), "Kind is still populated")
	assert.Empty(t, pl.Audience.Requester.ExternalID, "ExternalID stays empty: no raw id on record")
	assert.Empty(t, pl.Audience.Requester.Email, "Email stays empty: no verified email on record")
	assert.Equal(t, testStarter, pl.Audience.Requester.Subject.String(), "Subject = precomputed canonical, byte-identical fallback")
	require.NoError(t, pl.Validate(), "a Subject-only requester must still be wire-valid")
}

// TestCredentialRequestWatcher_MissingCredentialsVerbatim is the
// defense-against-future-regression test for the user-stated UX
// requirement: "list ALL accounts the user has not already connected."
// Five missing credentials must reach BOTH the envelope's What field
// and the signed link's RequiredCredentials field — no filtering, no
// truncation.
func TestCredentialRequestWatcher_MissingCredentialsVerbatim(t *testing.T) {
	missing := []string{"GitHub", "Linear", "Slack", "Jira", "Notion"}
	// The Item Titles MAY differ from MissingCredentials in principle —
	// the operator resolves them per-credential — but for this test the
	// contract is "every entry appears at every layer", so we line them
	// up 1:1 to make the assertions sharp.
	what := []string{"GitHub", "Linear", "Slack", "Jira", "Notion"}

	sess := fixtureSession(t)
	sui := fixtureSUI(missing, what, "Connect every service the agent needs.")
	w, _, pub := newTestWatcher(t, sess, sui)

	require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")
	pl := pub.findInteractionRequest(t)

	// Fields: every single entry, in order, no filtering.
	require.Len(t, pl.Fields, len(what), "payload.Fields length matches explanation.Items length")
	for i, title := range what {
		assert.Equal(t, title, pl.Fields[i].Label, "Fields[%d].Label verbatim", i)
	}

	// Signed link: same.
	require.Len(t, pl.Actions, 1, "exactly one action")
	decoded := decodeLinkPayload(t, w.LinkSigner, pl.Actions[0].URL)
	require.Len(t, decoded.RequiredCredentials, len(missing),
		"signed link RequiredCredentials length matches SUI missingCredentials length")
	assert.Equal(t, missing, decoded.RequiredCredentials, "RequiredCredentials verbatim")
}

// --- skip / requeue tests ----------------------------------------------

func TestCredentialRequestWatcher_SkipAndRequeue(t *testing.T) {
	cases := []struct {
		name string
		// build returns (session, sui-or-nil). When sui is nil the
		// fake client has no SUI for the session.
		build func(t *testing.T) (*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.SessionUserIdentity)
		// expectCondition: should the dedup condition be True after?
		expectCondition bool
	}{
		{
			name: "wrong phase: Running session is a noop",
			build: func(t *testing.T) (*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.SessionUserIdentity) {
				s := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
					s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
				})
				return s, fixtureSUI([]string{"GitHub"}, []string{"GitHub"}, "why")
			},
			expectCondition: false,
		},
		{
			name: "SUI not found: requeue, no envelope, no condition",
			build: func(t *testing.T) (*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.SessionUserIdentity) {
				return fixtureSession(t), nil
			},
			expectCondition: false,
		},
		{
			name: "explanation unset: requeue, no envelope, no condition",
			build: func(t *testing.T) (*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.SessionUserIdentity) {
				return fixtureSession(t), fixtureSUI([]string{"GitHub"}, nil, "")
			},
			expectCondition: false,
		},
		{
			name: "empty missingCredentials: skip, no envelope, no condition (operator-bug case)",
			build: func(t *testing.T) (*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.SessionUserIdentity) {
				// Note: explanation IS stamped, but missingCredentials is empty
				// — the operator shouldn't have parked but did.
				return fixtureSession(t), fixtureSUI(nil, []string{"GitHub"}, "why")
			},
			expectCondition: false,
		},
		{
			name: "no started-by annotation: skip, no envelope, no condition",
			build: func(t *testing.T) (*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.SessionUserIdentity) {
				s := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
					delete(s.Annotations, spiceboxv1alpha1.AnnotationStartedByCanonicalID)
				})
				return s, fixtureSUI([]string{"GitHub"}, []string{"GitHub"}, "why")
			},
			expectCondition: false,
		},
		{
			name: "session not channel-attached: skip (InputChannel nil)",
			build: func(t *testing.T) (*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.SessionUserIdentity) {
				s := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
					s.Spec.InputChannel = nil
					// Remove the label too so reconcileAll wouldn't even pick it
					// up — but ReconcileOne defends independently.
					delete(s.Labels, spiceboxv1alpha1.LabelChannelName)
				})
				return s, fixtureSUI([]string{"GitHub"}, []string{"GitHub"}, "why")
			},
			expectCondition: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess, sui := tc.build(t)
			objs := []client.Object{sess}
			if sui != nil {
				objs = append(objs, sui)
			}
			w, cli, pub := newTestWatcher(t, objs...)

			require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")

			assert.Equal(t, 0, pub.countInteractionRequests(), "no envelope published")

			var refetched spiceboxv1alpha1.AgentSession
			require.NoError(t, cli.Get(context.Background(),
				client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &refetched),
				"Get session")
			if tc.expectCondition {
				assert.True(t, alreadyPublished(&refetched), "condition True")
			} else {
				assert.False(t, alreadyPublished(&refetched), "condition NOT stamped")
			}
		})
	}
}

// TestCredentialRequestWatcher_NilNATSPublishFailsLoud pins the wiring-bug
// guard: doPublish requires NATSPublish, its only delivery path, and every
// channel kind implements interaction_request, so there is no
// degrade-gracefully case. A nil publisher must return a loud error rather
// than silently do nothing — silence here is a hang with no prompt ever
// rendered. The dedup condition must NOT be stamped, so a later tick (once an
// operator fixes the wiring) retries automatically.
func TestCredentialRequestWatcher_NilNATSPublishFailsLoud(t *testing.T) {
	sess := fixtureSession(t)
	sui := fixtureSUI([]string{"GitHub"}, []string{"GitHub"}, "why")
	w, cli, _ := newTestWatcher(t, sess, sui)
	w.NATSPublish = nil // simulate the wiring bug directly

	err := w.ReconcileOne(context.Background(), sess)
	require.Error(t, err, "ReconcileOne must error when NATSPublish is nil")
	assert.Contains(t, err.Error(), "NATSPublish not configured", "error names the wiring gap")

	var refetched spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &refetched), "Get")
	assert.False(t, alreadyPublished(&refetched),
		"condition NOT stamped: nothing was delivered, so the next tick must retry")
}

// TestCredentialRequestWatcher_AlreadyPublishedNoop verifies that a
// session whose CredentialRequestPublished condition is already True
// is a no-op on a fresh reconcile.
func TestCredentialRequestWatcher_AlreadyPublishedNoop(t *testing.T) {
	sess := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
		s.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.AgentSessionConditionCredentialRequestPublished,
			Status:             metav1.ConditionTrue,
			Reason:             spiceboxv1alpha1.ReasonCredentialRequestPublished,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}}
	})
	sui := fixtureSUI([]string{"GitHub"}, []string{"GitHub"}, "why")
	w, _, pub := newTestWatcher(t, sess, sui)

	require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")

	assert.Equal(t, 0, pub.countInteractionRequests(), "no envelope published")
}

// TestCredentialRequestWatcher_AgentClassTimeoutOverride verifies that
// when AgentClass.Spec.CredentialLinkTimeout is set the minted link's
// ExpiresAt uses that value instead of the default 30m.
func TestCredentialRequestWatcher_AgentClassTimeoutOverride(t *testing.T) {
	sess := fixtureSession(t)
	sui := fixtureSUI([]string{"GitHub"}, []string{"GitHub"}, "why")
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: testClass, Namespace: testNS},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			CredentialLinkTimeout: &metav1.Duration{Duration: 90 * time.Minute},
		},
	}
	w, _, pub := newTestWatcher(t, sess, sui, class)

	require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")
	pl := pub.findInteractionRequest(t)

	require.Len(t, pl.Actions, 1, "exactly one action")
	decoded := decodeLinkPayload(t, w.LinkSigner, pl.Actions[0].URL)
	expected := w.Now().Add(90 * time.Minute).Unix()
	assert.Equal(t, expected, decoded.ExpiresAt,
		"ExpiresAt reflects AgentClass.Spec.CredentialLinkTimeout, not default")
}

// TestCredentialRequestWatcher_ReconcileAllScansAndFiltersByPhase
// exercises the polling-loop entry to verify it picks up only the
// AwaitingCredentials sessions and skips the others.
func TestCredentialRequestWatcher_ReconcileAllScansAndFiltersByPhase(t *testing.T) {
	// Two sessions: one AwaitingCredentials (should fire), one Running
	// (should not). Both same kind/channel.
	awaiting := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
		s.Name = "sess-await"
	})
	running := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
		s.Name = "sess-running"
		s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	})
	awaitingSUI := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-await", Namespace: testNS},
		Spec:       spiceboxv1alpha1.SessionUserIdentitySpec{AgentSession: "sess-await", Subject: testStarter},
		Status: spiceboxv1alpha1.SessionUserIdentityStatus{
			MissingCredentials: []string{"GitHub"},
			Explanation: &spiceboxv1alpha1.CredentialExplanation{
				Items: []spiceboxv1alpha1.CredentialExplanationItem{
					{Credential: "GitHub", Title: "GitHub", Why: "why"},
				},
			},
		},
	}

	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(awaiting, running, awaitingSUI).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SessionUserIdentity{}).
		Build()
	pub := &capturingPublisher{}
	w := &CredentialRequestWatcher{
		K8s:             cli,
		LinkSigner:      newSigner(t),
		ExternalBaseURL: func() string { return "https://identityd.example.test" },
		NATSPublish:     pub.publish,
		Now:             fixedFutureNow,
	}

	// reconcileAll is package-private; we call it via a context.
	w.reconcileAll(context.Background(), discardLogger(t))

	require.Equal(t, 1, pub.countInteractionRequests(), "exactly one envelope (the AwaitingCredentials session)")
	pl := pub.findInteractionRequest(t)
	assert.Equal(t, "sess-await", pl.AgentSessionRef.Name,
		"envelope is for the AwaitingCredentials session")
}

// discardLogger returns a logr.Logger that swallows output but is
// satisfied to the watcher's signature.
func discardLogger(t *testing.T) logrLogger {
	t.Helper()
	return logrLogger{}
}

// logrLogger is a minimal in-test implementation: it is the actual
// logr.Logger struct (zero value is a discard sink).
type logrLogger = logr.Logger

// --- explainer wiring tests -------------------------------------------
//
// These tests verify the γ3 behaviour: the watcher calls the explainer
// before publishing, uses LLM output on success, and falls back to the
// static explanation on any error.

const testInitiatingMessage = "Can you open a PR fixing the bug in the Linear ticket?"

// fixtureSessionWithPrompt returns a fixtureSession with Spec.Prompt.Inline
// set to the given text — the source of the initiating message the
// explainer receives.
func fixtureSessionWithPrompt(t *testing.T, prompt string, mutate ...func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	all := append([]func(*spiceboxv1alpha1.AgentSession){
		func(s *spiceboxv1alpha1.AgentSession) {
			s.Spec.Prompt = spiceboxv1alpha1.PromptSource{Inline: prompt}
		},
	}, mutate...)
	return fixtureSession(t, all...)
}

// newExplainerWatcher is like newTestWatcher but wires an Explainer.
// Callers pass the fake's scripted response via the fakeexplainer.Explainer.
func newExplainerWatcher(t *testing.T, exp *fakeexplainer.Explainer, objs ...client.Object) (*CredentialRequestWatcher, *capturingPublisher) {
	t.Helper()
	w, _, pub := newTestWatcher(t, objs...)
	w.Explainer = exp
	return w, pub
}

// TestCredentialRequestWatcher_ExplainerTests is the table-driven suite
// that covers the watcher's LLM-explainer gap-fill and all fallback paths.
//
// Under the structured-Items model: Title always comes from the
// deterministic Item.Title (never the LLM); the LLM only fills EMPTY whys
// (the gaps), and only its Why output is consumed. The SUI is seeded with
// empty whys so every credential is a gap.
func TestCredentialRequestWatcher_ExplainerTests(t *testing.T) {
	staticWhat := []string{"GitHub", "Linear"}
	// No AgentClass is wired in these tests (acFound=false), so the
	// static gap fallback is the agent-neutral sentence.

	cases := []struct {
		name  string
		exp   *fakeexplainer.Explainer
		check func(t *testing.T, pl channelevents.InteractionRequestPayload, exp *fakeexplainer.Explainer)
	}{
		{
			name: "LLM happy path: Why from LLM, Title stays deterministic",
			exp: &fakeexplainer.Explainer{
				Response: explainer.Output{
					// out.What is IGNORED — titles are operator-owned now.
					What: []string{"GitHub (LLM)", "Linear (LLM)"},
					Why: []string{
						"To open the PR you asked about.",
						"To update the linked ticket's status.",
					},
				},
			},
			check: func(t *testing.T, pl channelevents.InteractionRequestPayload, exp *fakeexplainer.Explainer) {
				require.Len(t, pl.Fields, 2, "Fields has one entry per SUI explanation item")
				assert.Equal(t, staticWhat[0], pl.Fields[0].Label, "Fields[0].Label stays deterministic (not LLM)")
				assert.Equal(t, staticWhat[1], pl.Fields[1].Label, "Fields[1].Label stays deterministic (not LLM)")
				assert.Equal(t, "To open the PR you asked about.", pl.Fields[0].Value, "LLM per-service Why used to fill the gap")
				assert.Equal(t, "To update the linked ticket's status.", pl.Fields[1].Value, "LLM per-service Why used to fill the gap")
				calls := exp.Calls()
				require.Len(t, calls, 1, "Explain called once")
				// Prompt-injection boundary: the Input contains the
				// initiating message and credential metadata only — no
				// agent output or tool output.
				assert.Equal(t, testInitiatingMessage, calls[0].InitiatingMessage,
					"Input.InitiatingMessage is the session prompt")
				assert.Len(t, calls[0].Credentials, 2, "Input.Credentials has one entry per gap cred")
			},
		},
		{
			name: "LLM error: gap whys fall back to static",
			exp: &fakeexplainer.Explainer{
				ResponseErr: errors.New("anthropic: 503 Service Unavailable"),
			},
			check: func(t *testing.T, pl channelevents.InteractionRequestPayload, _ *fakeexplainer.Explainer) {
				require.Len(t, pl.Fields, 2, "Fields has one entry per SUI explanation item")
				assert.Equal(t, staticWhat[0], pl.Fields[0].Label, "deterministic Label on LLM error")
				assert.Equal(t, staticWhat[1], pl.Fields[1].Label, "deterministic Label on LLM error")
				assert.Equal(t, staticGapWhy, pl.Fields[0].Value, "static Why on LLM error")
				assert.Equal(t, staticGapWhy, pl.Fields[1].Value, "static Why on LLM error")
			},
		},
		{
			name: "LLM ErrEmptyResponse: gap whys fall back to static",
			exp: &fakeexplainer.Explainer{
				ResponseErr: explainer.ErrEmptyResponse,
			},
			check: func(t *testing.T, pl channelevents.InteractionRequestPayload, _ *fakeexplainer.Explainer) {
				require.Len(t, pl.Fields, 2, "Fields has one entry per SUI explanation item")
				assert.Equal(t, staticWhat[0], pl.Fields[0].Label, "deterministic Label on ErrEmptyResponse")
				assert.Equal(t, staticWhat[1], pl.Fields[1].Label, "deterministic Label on ErrEmptyResponse")
				assert.Equal(t, staticGapWhy, pl.Fields[0].Value, "static Why on ErrEmptyResponse")
				assert.Equal(t, staticGapWhy, pl.Fields[1].Value, "static Why on ErrEmptyResponse")
			},
		},
		{
			name: "LLM returns wrong-cardinality Why: gap whys fall back to static",
			exp: &fakeexplainer.Explainer{
				// Two gaps but LLM returns only one Why entry.
				Response: explainer.Output{
					What: []string{"GitHub (LLM)"},
					Why:  []string{"truncated"},
				},
			},
			check: func(t *testing.T, pl channelevents.InteractionRequestPayload, _ *fakeexplainer.Explainer) {
				// Must use the static fallback — NOT the truncated LLM
				// output — to satisfy the UX requirement that EVERY row
				// carries a reason.
				require.Len(t, pl.Fields, 2, "Fields has one entry per SUI explanation item")
				assert.Equal(t, staticWhat[0], pl.Fields[0].Label, "deterministic Label")
				assert.Equal(t, staticWhat[1], pl.Fields[1].Label, "deterministic Label")
				assert.Equal(t, staticGapWhy, pl.Fields[0].Value,
					"falls back to static when LLM Why length != gap count")
				assert.Equal(t, staticGapWhy, pl.Fields[1].Value,
					"falls back to static when LLM Why length != gap count")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := fixtureSessionWithPrompt(t, testInitiatingMessage)
			// Empty whyText ⇒ every credential is a gap the explainer fills.
			sui := fixtureSUI(staticWhat, staticWhat, "")
			w, pub := newExplainerWatcher(t, tc.exp, sess, sui)
			require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")
			pl := pub.findInteractionRequest(t)
			tc.check(t, pl, tc.exp)
		})
	}
}

// TestCredentialRequestWatcher_NilExplainerUsesStatic verifies that when
// no Explainer is configured and the SUI carries no declared Why, the
// watcher fills the gap with the static sentence — no panic, no error.
func TestCredentialRequestWatcher_NilExplainerUsesStatic(t *testing.T) {
	what := []string{"GitHub"}
	sess := fixtureSession(t)
	// Empty whyText ⇒ a gap with no declared reason and no explainer.
	sui := fixtureSUI(what, what, "")
	w, _, pub := newTestWatcher(t, sess, sui)
	// Explainer is nil by default in newTestWatcher.
	require.Nil(t, w.Explainer, "no explainer wired")

	require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")
	pl := pub.findInteractionRequest(t)
	require.Len(t, pl.Fields, 1, "Fields has one entry")
	assert.Equal(t, what[0], pl.Fields[0].Label, "Fields[0].Label from Item.Title")
	// Per-service Why: the static gap sentence (no declared reason, no LLM).
	assert.Equal(t, staticGapWhy, pl.Fields[0].Value, "static per-service Why")
}

// --- OAuth-aware LinkButtons ------------------------------------------
//
// The watcher partitions missing credentials by each source MCPServer's
// Spec.Auth.Type, producing OAuth-per-button + shared-PAT-button output. The
// invariant across all of them: no truncation — every missing credential is
// covered, or the user cannot finish connecting.

// mcpServerOAuth builds a minimal MCPServer marked OAuth.
func mcpServerOAuth(t *testing.T, ns, name, credName, provider string) *spiceboxv1alpha1.MCPServer {
	t.Helper()
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    name,
			Version: "1.0",
			Server: spiceboxv1alpha1.MCPServerServer{
				URL:       "https://" + name + ".example/mcp",
				Transport: "http",
			},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Type:       "oauth",
				Credential: credName,
				Provider:   provider,
			},
		},
	}
}

// mcpServerPAT builds a minimal MCPServer marked static (or empty,
// which the catalog treats as PAT).
func mcpServerPAT(t *testing.T, ns, name, credName string) *spiceboxv1alpha1.MCPServer {
	t.Helper()
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    name,
			Version: "1.0",
			Server: spiceboxv1alpha1.MCPServerServer{
				URL:       "https://" + name + ".example/mcp",
				Transport: "http",
			},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Type:       "static",
				Credential: credName,
			},
		},
	}
}

// mcpServerPATProvider is mcpServerPAT with an explicit Provider —
// exercises the single-button label rule for PAT credentials that
// have a real MCPServer (vs. envvar-only PAT bindings).
func mcpServerPATProvider(t *testing.T, ns, name, credName, provider string) *spiceboxv1alpha1.MCPServer {
	t.Helper()
	srv := mcpServerPAT(t, ns, name, credName)
	srv.Spec.Auth.Provider = provider
	return srv
}

// TestCredentialRequestWatcher_OneButton_AlwaysSingleEntry pins the
// invariant added when we collapsed the multi-button render into a
// single channel button (the per-credential routing moved into
// identityd's /link menu page). Regardless of the mix of OAuth vs
// PAT credentials in the session, exactly one button appears.
func TestCredentialRequestWatcher_OneButton_AlwaysSingleEntry(t *testing.T) {
	cases := []struct {
		name    string
		missing []string
		what    []string
		mcps    []client.Object
	}{
		{
			name:    "mixed OAuth + PAT",
			missing: []string{"linear-oauth", "github-oauth", "stripe-pat", "notion-pat", "jira-pat"},
			what:    []string{"Linear", "GitHub", "Stripe", "Notion", "Jira"},
			mcps: []client.Object{
				mcpServerOAuth(t, testNS, "linear-mcp", "linear-oauth", "linear"),
				mcpServerOAuth(t, testNS, "github-mcp", "github-oauth", "github"),
				mcpServerPAT(t, testNS, "stripe-mcp", "stripe-pat"),
				mcpServerPAT(t, testNS, "notion-mcp", "notion-pat"),
				mcpServerPAT(t, testNS, "jira-mcp", "jira-pat"),
			},
		},
		{
			name:    "all OAuth",
			missing: []string{"linear-oauth", "github-oauth", "slack-oauth"},
			what:    []string{"Linear", "GitHub", "Slack"},
			mcps: []client.Object{
				mcpServerOAuth(t, testNS, "linear-mcp", "linear-oauth", "linear"),
				mcpServerOAuth(t, testNS, "github-mcp", "github-oauth", "github"),
				mcpServerOAuth(t, testNS, "slack-mcp", "slack-oauth", "slack"),
			},
		},
		{
			name:    "all PAT",
			missing: []string{"stripe-pat", "notion-pat", "jira-pat"},
			what:    []string{"Stripe", "Notion", "Jira"},
			mcps: []client.Object{
				mcpServerPAT(t, testNS, "stripe-mcp", "stripe-pat"),
				mcpServerPAT(t, testNS, "notion-mcp", "notion-pat"),
				mcpServerPAT(t, testNS, "jira-mcp", "jira-pat"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{fixtureSession(t), fixtureSUI(tc.missing, tc.what, "why")}, tc.mcps...)
			w, _, pub := newTestWatcher(t, objs...)
			sess := objs[0].(*spiceboxv1alpha1.AgentSession)
			require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")
			pl := pub.findInteractionRequest(t)

			require.Len(t, pl.Actions, 1, "exactly one action regardless of OAuth/PAT mix")
			action := pl.Actions[0]
			assert.Equal(t, "Connect your accounts", action.Label,
				"multi-credential case uses the generic label; the menu page enumerates them")
			assert.Equal(t, channelevents.ActionKindLink, action.Kind, "action.Kind")
			require.True(t, strings.HasPrefix(action.URL, "https://identityd.example.test/link?"),
				"action URL points at the /link menu page (signed): %s", action.URL)

			decoded := decodeLinkPayload(t, w.LinkSigner, action.URL)
			assert.Equal(t, tc.missing, decoded.RequiredCredentials,
				"signed link carries the COMPLETE missing list (menu page renders per-row routing)")
		})
	}
}

// TestCredentialRequestWatcher_SingleCredentialUsesProviderLabel pins
// the UX rule: with exactly one missing credential the channel button
// label names the provider concretely so the user knows what they're
// about to connect.
func TestCredentialRequestWatcher_SingleCredentialUsesProviderLabel(t *testing.T) {
	cases := []struct {
		name      string
		missing   []string
		mcpServer client.Object
		wantLabel string
	}{
		{
			name:      "OAuth credential with explicit Provider",
			missing:   []string{"linear-oauth"},
			mcpServer: mcpServerOAuth(t, testNS, "linear-mcp", "linear-oauth", "Linear"),
			wantLabel: "Connect Linear",
		},
		{
			name:      "PAT credential with explicit Provider",
			missing:   []string{"github-pat"},
			mcpServer: mcpServerPATProvider(t, testNS, "github-mcp", "github-pat", "GitHub"),
			wantLabel: "Connect GitHub",
		},
		{
			name:      "no MCPServer → humanized credential name",
			missing:   []string{"github-token"},
			mcpServer: nil,
			wantLabel: "Connect Github Token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{
				fixtureSession(t),
				fixtureSUI(tc.missing, []string{"x"}, "why"),
			}
			if tc.mcpServer != nil {
				objs = append(objs, tc.mcpServer)
			}
			w, _, pub := newTestWatcher(t, objs...)
			sess := objs[0].(*spiceboxv1alpha1.AgentSession)
			require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")
			pl := pub.findInteractionRequest(t)

			require.Len(t, pl.Actions, 1)
			assert.Equal(t, tc.wantLabel, pl.Actions[0].Label)
		})
	}
}

// TestCredentialRequestWatcher_SignedLinkCoversEveryMissing: the channel
// renders one "Connect your accounts" button, but the signed link's
// RequiredCredentials MUST enumerate EVERY missing credential — the menu page
// renders one row per entry, so anything dropped here is an account the user
// is never offered a way to connect.
func TestCredentialRequestWatcher_SignedLinkCoversEveryMissing(t *testing.T) {
	missing := []string{
		"linear-oauth",
		"github-oauth",
		"slack-oauth",
		"notion-oauth",
		"hubspot-oauth",
	}
	what := []string{"Linear", "GitHub", "Slack", "Notion", "HubSpot"}

	objs := []client.Object{
		fixtureSession(t),
		fixtureSUI(missing, what, "five oauth providers"),
		mcpServerOAuth(t, testNS, "linear-mcp", "linear-oauth", "linear"),
		mcpServerOAuth(t, testNS, "github-mcp", "github-oauth", "github"),
		mcpServerOAuth(t, testNS, "slack-mcp", "slack-oauth", "slack"),
		mcpServerOAuth(t, testNS, "notion-mcp", "notion-oauth", "notion"),
		mcpServerOAuth(t, testNS, "hubspot-mcp", "hubspot-oauth", "hubspot"),
	}

	w, _, pub := newTestWatcher(t, objs...)
	sess := objs[0].(*spiceboxv1alpha1.AgentSession)
	require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")
	pl := pub.findInteractionRequest(t)

	require.Len(t, pl.Actions, 1)
	decoded := decodeLinkPayload(t, w.LinkSigner, pl.Actions[0].URL)
	assert.ElementsMatch(t, missing, decoded.RequiredCredentials,
		"every missing credential MUST appear in the signed link's RequiredCredentials; no truncation")
}

// TestCredentialRequestWatcher_ExplainerPromptInjectionBoundary asserts
// that the Input seen by the explainer contains ONLY the human's
// initiating message + static credential metadata — never agent output,
// tool output, or any other dynamic LLM content. This is a regression
// guard for the prompt-injection threat model.
func TestCredentialRequestWatcher_ExplainerPromptInjectionBoundary(t *testing.T) {
	// The initiating message is the only attacker-controlled input the
	// explainer should see. Script the fake to record calls and succeed.
	exp := &fakeexplainer.Explainer{
		Response: explainer.Output{
			What: []string{"GitHub", "Linear"},
			Why:  []string{"some why", "another why"},
		},
	}
	initiating := "Please help me draft a reply to the GitHub issue."
	sess := fixtureSessionWithPrompt(t, initiating)
	// Empty whyText ⇒ both credentials are gaps the explainer is asked
	// to fill, so the explainer is actually invoked.
	sui := fixtureSUI(
		[]string{"gh-oauth", "linear-oauth"},
		[]string{"GitHub", "Linear"},
		"",
	)
	w, pub := newExplainerWatcher(t, exp, sess, sui)
	require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")
	require.Equal(t, 1, pub.countInteractionRequests(), "envelope published")

	calls := exp.Calls()
	require.Len(t, calls, 1, "Explain called once")
	in := calls[0]

	// The initiating message and only the initiating message is the
	// attacker-controlled input.
	assert.Equal(t, initiating, in.InitiatingMessage,
		"Input.InitiatingMessage must be exactly the session Spec.Prompt.Inline")

	// Credentials come from static metadata, not from agent output.
	require.Len(t, in.Credentials, 2, "one CredentialInfo per missing credential")
	assert.Equal(t, "gh-oauth", in.Credentials[0].Name)
	assert.Equal(t, "GitHub", in.Credentials[0].Provider)
	assert.Equal(t, "linear-oauth", in.Credentials[1].Name)
	assert.Equal(t, "Linear", in.Credentials[1].Provider)
}

// capturingPublisher records every (subject, data) published through it so
// tests can assert on the MonitoringEvent + notification the fail-closed
// path emits. ReconcileOne runs synchronously in tests, so no locking.
type capturingPublisher struct {
	messages []capturedMessage
}

type capturedMessage struct {
	subject string
	data    []byte
}

func (p *capturingPublisher) publish(subject string, data []byte) error {
	p.messages = append(p.messages, capturedMessage{subject: subject, data: append([]byte(nil), data...)})
	return nil
}

// findMonitoring returns the single MonitoringEvent published on the
// monitoring subject, failing the test when none was emitted.
func (p *capturingPublisher) findMonitoring(t *testing.T) channelevents.MonitoringEvent {
	t.Helper()
	for _, m := range p.messages {
		if m.subject == channelevents.MonitoringEventSubject {
			var ev channelevents.MonitoringEvent
			require.NoError(t, json.Unmarshal(m.data, &ev), "unmarshal MonitoringEvent")
			return ev
		}
	}
	t.Fatalf("no MonitoringEvent published on %s", channelevents.MonitoringEventSubject)
	return channelevents.MonitoringEvent{}
}

// countMonitoring returns how many MonitoringEvents were published — used to
// assert the dedup (the noisy surfaces fire only on the first transition).
func (p *capturingPublisher) countMonitoring() int {
	n := 0
	for _, m := range p.messages {
		if m.subject == channelevents.MonitoringEventSubject {
			n++
		}
	}
	return n
}

// hasNonMonitoring reports whether any message was published on a subject
// other than the monitoring subject — i.e. the session-visible
// KindNotification went out.
func (p *capturingPublisher) hasNonMonitoring() bool {
	for _, m := range p.messages {
		if m.subject != channelevents.MonitoringEventSubject {
			return true
		}
	}
	return false
}

// envelopesOfKind decodes every captured message as a channelevents.Envelope
// and returns the ones matching k. Messages that aren't Envelope-shaped
// (e.g. a MonitoringEvent, published as a bare struct rather than wrapped in
// an Envelope) unmarshal to a zero-value Envelope whose Kind never matches a
// real Kind constant, so they're naturally filtered out — no subject
// filtering needed.
func (p *capturingPublisher) envelopesOfKind(k channelevents.Kind) []channelevents.Envelope {
	var out []channelevents.Envelope
	for _, m := range p.messages {
		var env channelevents.Envelope
		if err := json.Unmarshal(m.data, &env); err != nil {
			continue
		}
		if env.Kind == k {
			out = append(out, env)
		}
	}
	return out
}

// interactionRequests decodes every published KindInteractionRequest
// envelope's payload.
func (p *capturingPublisher) interactionRequests(t *testing.T) []channelevents.InteractionRequestPayload {
	t.Helper()
	envs := p.envelopesOfKind(channelevents.KindInteractionRequest)
	out := make([]channelevents.InteractionRequestPayload, len(envs))
	for i, env := range envs {
		require.NoError(t, json.Unmarshal(env.Payload, &out[i]), "unmarshal InteractionRequestPayload[%d]", i)
	}
	return out
}

// findInteractionRequest is interactionRequests asserting there is exactly
// one — the shape every single-publish test in this file wants.
func (p *capturingPublisher) findInteractionRequest(t *testing.T) channelevents.InteractionRequestPayload {
	t.Helper()
	reqs := p.interactionRequests(t)
	require.Len(t, reqs, 1, "expected exactly one KindInteractionRequest published")
	return reqs[0]
}

// countInteractionRequests reports how many KindInteractionRequest
// envelopes have been published — used by dedup assertions in place of the
// old resolver.CallCount / sender.sendCount().
func (p *capturingPublisher) countInteractionRequests() int {
	return len(p.envelopesOfKind(channelevents.KindInteractionRequest))
}

// TestCredentialRequestWatcher_FailsClosedWhenExternalURLUnconfigured is the
// core fail-closed guard: when the platform's external web address is
// unconfigured (ExternalBaseURL returns ""), the watcher must NOT send a
// link button. Instead it must (a) stamp CredentialLinkAvailable=False on
// the SUI, (b) publish a MonitoringEvent, (c) emit a session-visible
// notification, and (d) return an error so the next tick retries. It must
// NOT stamp the CredentialRequestPublished dedup condition, so recovery is
// automatic once the operator fixes the ConfigMap. The noisy surfaces
// (b + c) fire only on the FIRST transition, not every 5s tick.
func TestCredentialRequestWatcher_FailsClosedWhenExternalURLUnconfigured(t *testing.T) {
	sess := fixtureSession(t)
	sui := fixtureSUI([]string{"GitHub", "Linear"}, []string{"GitHub", "Linear"}, "why")
	w, cli, pub := newTestWatcher(t, sess, sui)
	w.ExternalBaseURL = func() string { return "" } // fail-closed signal

	// First reconcile: fail closed.
	err := w.ReconcileOne(context.Background(), sess)
	require.Error(t, err, "ReconcileOne must return an error when the external URL is unconfigured")
	assert.Contains(t, err.Error(), "external URL not configured", "error names the unconfigured external URL")

	// No link: the mint/publish path is never reached.
	assert.Equal(t, 0, pub.countInteractionRequests(), "no interaction_request published on the fail-closed path")

	// a. SUI condition CredentialLinkAvailable=False / WebdExternalURLNotConfigured.
	var gotSUI spiceboxv1alpha1.SessionUserIdentity
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &gotSUI), "Get SUI")
	cond := meta.FindStatusCondition(gotSUI.Status.Conditions,
		spiceboxv1alpha1.SessionUserIdentityConditionCredentialLinkAvailable)
	require.NotNil(t, cond, "CredentialLinkAvailable condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "CredentialLinkAvailable=False")
	assert.Equal(t, spiceboxv1alpha1.ReasonWebdExternalURLNotConfigured, cond.Reason, "reason")
	assert.Contains(t, cond.Message, spiceboxv1alpha1.WebdExternalURLConfigMap,
		"message names the ConfigMap to fix")

	// b. Monitoring event on the monitoring subject.
	ev := pub.findMonitoring(t)
	assert.Equal(t, channelevents.MonitoringLevelError, ev.Level, "monitoring level")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, ev.Transition, "monitoring transition")
	assert.Equal(t, "credential", ev.Category, "monitoring category")
	assert.Equal(t, "AgentSession", ev.Source.Kind, "monitoring source kind")
	assert.Equal(t, sess.Name, ev.Source.Name, "monitoring source name")
	assert.Equal(t, spiceboxv1alpha1.SessionUserIdentityConditionCredentialLinkAvailable, ev.Condition, "monitoring condition")
	assert.Equal(t, spiceboxv1alpha1.ReasonWebdExternalURLNotConfigured, ev.Reason, "monitoring reason")
	assert.NotEmpty(t, ev.Hint, "monitoring hint guides remediation")

	// c. Session-visible notification (any non-monitoring publish).
	assert.True(t, pub.hasNonMonitoring(), "a session-visible notification must be published")

	// Dedup: CredentialRequestPublished must NOT be stamped → next tick retries.
	var gotSess spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &gotSess), "Get session")
	assert.False(t, alreadyPublished(&gotSess),
		"CredentialRequestPublished must NOT be stamped on the fail-closed path")

	// Second reconcile while still unconfigured: the SUI condition keeps it
	// at exactly one MonitoringEvent (no per-tick spam), still no send, still
	// an error.
	err = w.ReconcileOne(context.Background(), &gotSess)
	require.Error(t, err, "still fails closed on the second tick")
	assert.Equal(t, 0, pub.countInteractionRequests(), "still no interaction_request on the second tick")
	assert.Equal(t, 1, pub.countMonitoring(),
		"monitoring event fires only on the first transition, not every tick")
}

// TestCredentialRequestWatcher_RecoversAfterExternalURLConfigured verifies
// the fail-closed path is transient: once ExternalBaseURL returns a real
// URL, the next ReconcileOne mints and sends the link normally.
func TestCredentialRequestWatcher_RecoversAfterExternalURLConfigured(t *testing.T) {
	sess := fixtureSession(t)
	sui := fixtureSUI([]string{"GitHub"}, []string{"GitHub"}, "why")
	w, cli, pub := newTestWatcher(t, sess, sui)

	url := ""
	w.ExternalBaseURL = func() string { return url }

	// Unconfigured: fails closed, no send.
	require.Error(t, w.ReconcileOne(context.Background(), sess), "fails closed while unconfigured")
	assert.Equal(t, 0, pub.countInteractionRequests(), "no publish while unconfigured")

	// Operator configures the URL; re-read the (condition-stamped) session.
	url = "https://identityd.example.test"
	var gotSess spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &gotSess), "Get session")

	require.NoError(t, w.ReconcileOne(context.Background(), &gotSess), "recovers once URL is configured")
	require.Equal(t, 1, pub.countInteractionRequests(), "link published after recovery")
	pl := pub.findInteractionRequest(t)
	require.Len(t, pl.Actions, 1, "exactly one action")
	require.True(t, strings.HasPrefix(pl.Actions[0].URL, "https://identityd.example.test/link?"),
		"recovered link points at the configured URL: %s", pl.Actions[0].URL)
}

// TestBuildLinkURL exercises the defense-in-depth guard: buildLinkURL must
// refuse any base that isn't an absolute http(s) URL with a host, so no
// caller can ever emit a hostless "/link?…" credential button.
func TestBuildLinkURL(t *testing.T) {
	const raw = "eyJkIjoxfQ.c2ln" // <b64>.<sig> shape; contents irrelevant here
	cases := []struct {
		name       string
		base       string
		raw        string
		wantErr    bool
		wantPrefix string
	}{
		{name: "empty base: error (no hostless link)", base: "", raw: raw, wantErr: true},
		{name: "relative/hostless base: error", base: "/just/a/path", raw: raw, wantErr: true},
		{name: "scheme-less host: error", base: "webd.example.com", raw: raw, wantErr: true},
		{name: "non-http scheme: error", base: "ftp://webd.example.com", raw: raw, wantErr: true},
		{name: "malformed raw (no dot): error", base: "https://webd.example.com", raw: "nodot", wantErr: true},
		{name: "valid https: ok", base: "https://webd.example.com", raw: raw, wantErr: false,
			wantPrefix: "https://webd.example.com/link?"},
		{name: "valid http with trailing slash: ok and trimmed", base: "http://localhost:8080/", raw: raw, wantErr: false,
			wantPrefix: "http://localhost:8080/link?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLinkURL(tc.base, tc.raw)
			if tc.wantErr {
				require.Error(t, err, "expected an error for base=%q raw=%q", tc.base, tc.raw)
				assert.Empty(t, got, "no URL returned on error")
				return
			}
			require.NoError(t, err, "unexpected error for base=%q", tc.base)
			assert.True(t, strings.HasPrefix(got, tc.wantPrefix),
				"link %q must start with %q", got, tc.wantPrefix)
		})
	}
}

// --- ForcePublish (dedup-bypass re-surface) tests -----------------------
//
// ForcePublish re-sends the credential prompt when a user re-interacts
// while the session is still AwaitingCredentials, bypassing the
// CredentialRequestPublished dedup condition that ReconcileOne respects.
// It keeps ReconcileOne's phase/InputChannel/missing-credentials guards.

// newCredWatcherFixture builds a CredentialRequestWatcher wired against a
// fake k8s client seeded with an AwaitingCredentials AgentSession and a
// matching SessionUserIdentity with exactly one missing credential
// ("GitHub"). Reuses the package's existing fixtureSession/fixtureSUI/
// newTestWatcher builders. Returns the watcher, the capturingPublisher the
// watcher will publish through, and the session.
func newCredWatcherFixture(t *testing.T) (w *CredentialRequestWatcher, pub *capturingPublisher, sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	sess = fixtureSession(t)
	sui := fixtureSUI([]string{"GitHub"}, []string{"GitHub"}, "why")
	w, _, pub = newTestWatcher(t, sess, sui)
	return w, pub, sess
}

// setCredentialPublishedCondition stamps CredentialRequestPublished=True
// directly on the in-memory sess — mirroring what markPublished would have
// patched in after a prior publish — so ReconcileOne's dedup guard
// (alreadyPublished, which reads the passed-in struct, not a re-fetched
// copy) treats the session as already-sent.
func setCredentialPublishedCondition(sess *spiceboxv1alpha1.AgentSession) {
	conditions.SetTrue(sess, &sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionCredentialRequestPublished,
		spiceboxv1alpha1.ReasonCredentialRequestPublished)
}

// clearMissingCredentials patches the fake-client SUI backing sess down to
// zero missing credentials, exercising doPublish's "nothing missing"
// no-op guard through ForcePublish. Needs the watcher (for w.K8s) since
// MissingCredentials lives on the SessionUserIdentity, not the session.
func clearMissingCredentials(t *testing.T, w *CredentialRequestWatcher, sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	var sui spiceboxv1alpha1.SessionUserIdentity
	key := client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}
	require.NoError(t, w.K8s.Get(context.Background(), key, &sui), "get SUI")
	prior := sui.DeepCopy()
	sui.Status.MissingCredentials = nil
	require.NoError(t, w.K8s.Status().Patch(context.Background(), &sui, client.MergeFrom(prior)),
		"patch SUI missingCredentials to empty")
}

// TestForcePublishBypassesDedup verifies that ReconcileOne respects the
// CredentialRequestPublished dedup condition (no send) while ForcePublish
// ignores it and sends a fresh prompt.
func TestForcePublishBypassesDedup(t *testing.T) {
	w, pub, sess := newCredWatcherFixture(t)
	setCredentialPublishedCondition(sess) // dedup already stamped

	// ReconcileOne respects dedup: no publish.
	require.NoError(t, w.ReconcileOne(context.Background(), sess), "ReconcileOne")
	assert.Equal(t, 0, pub.countInteractionRequests(), "ReconcileOne must not publish when already published")

	// ForcePublish ignores dedup: one publish.
	require.NoError(t, w.ForcePublish(context.Background(), sess), "ForcePublish")
	assert.Equal(t, 1, pub.countInteractionRequests(), "ForcePublish publishes despite dedup condition")
}

// TestForcePublishSkipsMarkPublishedWhenAlreadyStamped verifies that a
// re-surface (ForcePublish) on an already-published session still sends the
// prompt but does NOT re-stamp the CredentialRequestPublished condition.
// Re-stamping would rewrite the condition Message with each freshly minted
// link URL, churning the session's status on every re-interaction.
func TestForcePublishSkipsMarkPublishedWhenAlreadyStamped(t *testing.T) {
	w, pub, sess := newCredWatcherFixture(t)
	setCredentialPublishedCondition(sess) // dedup already stamped (empty Message)

	require.NoError(t, w.ForcePublish(context.Background(), sess), "ForcePublish")
	assert.Equal(t, 1, pub.countInteractionRequests(), "ForcePublish still publishes on re-surface")

	// markPublished (which sets a "Delivered credential link…" Message that
	// embeds the link URL) must NOT have run: the persisted condition carries
	// no such Message, proving no status churn. Asserted against the client's
	// stored session (setCredentialPublishedCondition only touched the in-memory
	// struct), so this fails if markPublished re-patched the condition.
	var refetched spiceboxv1alpha1.AgentSession
	require.NoError(t, w.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &refetched), "refetch session")
	var msg string
	for _, c := range refetched.Status.Conditions {
		if c.Type == spiceboxv1alpha1.AgentSessionConditionCredentialRequestPublished {
			msg = c.Message
		}
	}
	assert.NotContains(t, msg, "Delivered credential link",
		"markPublished must be skipped on re-surface when already published (no status churn)")
}

// TestForcePublishSkipsWhenNothingMissing verifies ForcePublish is a
// no-op (no send, no error) when the SUI has no missing credentials —
// mirrors ReconcileOne's existing "operator bug" skip.
func TestForcePublishSkipsWhenNothingMissing(t *testing.T) {
	w, pub, sess := newCredWatcherFixture(t)
	clearMissingCredentials(t, w, sess) // SUI has no missing creds

	require.NoError(t, w.ForcePublish(context.Background(), sess), "ForcePublish")
	assert.Equal(t, 0, pub.countInteractionRequests(), "ForcePublish is a no-op when nothing is missing")
}
