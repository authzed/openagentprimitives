package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// --- matcher tests ------------------------------------------------------

func TestMatchesPortalTrigger(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"exact: manage my accounts", "manage my accounts", true},
		{"exact: link my accounts", "link my accounts", true},
		{"exact: !my/accounts", "!my/accounts", true},
		{"case-insensitive: MANAGE my Accounts", "MANAGE my Accounts", true},
		{"case-insensitive: LINK MY ACCOUNTS", "LINK MY ACCOUNTS", true},
		{"whitespace-tolerant: '  link my accounts  '", "  link my accounts  ", true},
		{"whitespace-tolerant: tab-prefixed manage", "\tmanage my accounts\n", true},
		{"non-trigger: hello", "hello", false},
		{"non-trigger: prefix only", "manage my", false},
		{"non-trigger: trailing garbage", "manage my accounts please", false},
		{"non-trigger: leading garbage", "please manage my accounts", false},
		{"non-trigger: empty", "", false},
		{"non-trigger: similar punctuation", "!my-accounts", false},
		{"non-trigger: agent prompt", "manage my accounts and also check the docs", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, matchesPortalTrigger(tc.text))
		})
	}
}

// --- happy-path test ----------------------------------------------------

const (
	portalTestStarter           = "user:alice"
	portalTestStarterExternalID = "U_ALICE"
	portalTestStarterEmail      = "alice@example.com"
	portalTestExternalURL       = "https://identityd.example.test"
	portalTestNS                = "default"
	portalTestSession           = "session-portal-aaaa1111"
	portalTestChannelName       = "ch-fake"
	portalTestKindName          = "fake"
)

// fixturePortalSession returns an active AgentSession with the started-by
// annotation set + an InputChannel binding — minimum shape the triggerer
// needs to mint a link and publish an interaction_request.
func fixturePortalSession(mutate ...func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      portalTestSession,
			Namespace: portalTestNS,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: portalTestStarter,
				spiceboxv1alpha1.AnnotationStartedByExternalID:  portalTestStarterExternalID,
				spiceboxv1alpha1.AnnotationStartedByEmail:       portalTestStarterEmail,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: portalTestChannelName,
				Kind: portalTestKindName,
				Key:  "dm:U_ALICE",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseRunning,
		},
	}
	for _, m := range mutate {
		m(sess)
	}
	return sess
}

// newPortalTriggerer wires a PortalAccessTriggerer against the supplied NATS
// publisher fake and the test signer. Now is fixed for deterministic ExpiresAt.
// A published interaction_request is the triggerer's only delivery path, so the
// harness wires NATS (a fakeNATS or erroringNATS, both defined package-wide in
// pipeline_test.go / decision_test.go) and no sub-channel sender.
func newPortalTriggerer(t *testing.T, nats NATS) *PortalAccessTriggerer {
	t.Helper()
	return &PortalAccessTriggerer{
		LinkSigner:      newSigner(t),
		ExternalBaseURL: func() string { return portalTestExternalURL },
		NATS:            nats,
		Now:             fixedFutureNow,
	}
}

// TestDeliverPortalAccess_PublishFailure_TellsTheUserInsteadOfStrandingThem pins
// that a portal trigger whose interaction_request never gets delivered does
// not leave the user staring at a spinner.
//
// TryHandle returns handled=true even on failure — deliberately, so the agent
// never sees the trigger phrase. Deliver must NOT then log and return
// OutcomeRouted: that posts the "Starting…" placeholder with no agent wakeup
// behind it and no link ever arriving, and 120s later the silence watchdog
// calls the session stalled. The user is waiting on something that will never
// come, so say so.
//
// The failure is injected as a NATS publish error (erroringNATS) because
// publish is the only delivery path, hence the only failure surface Deliver
// has to translate into a notice.
func TestDeliverPortalAccess_PublishFailure_TellsTheUserInsteadOfStrandingThem(t *testing.T) {
	ch := newChannel("c1")
	active := archivedSession(t, "c1-live", "thread:C1:9", spiceboxv1alpha1.AgentSessionPhaseRunning, "U1")
	active.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = "user:alice"
	// A binding is required, else TryHandle short-circuits at (true, nil) and
	// never reaches the publish failure this test is about.
	active.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name: "c1", Kind: "fake", Key: "thread:C1:9",
	}
	p, _, _, _, _ := newPipeline(t, ch, active)
	p.PortalAccess = newPortalTriggerer(t, &erroringNATS{err: errors.New("boom")})

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:9",
		MessageText: "manage my accounts",
	})
	require.NoError(t, err, "Deliver must not fail the inbound")

	assert.NotEqual(t, channelkinds.OutcomeRouted, dec.Outcome,
		"a failed portal publish must not masquerade as a routed turn with a starting… spinner")
	assert.NotEmpty(t, dec.Notice,
		"the user asked for a link that will never arrive; tell them")
}

// TestDeliverPortalAccess_Handled_AttachmentsNotCarried_UserToldAndLogged
// covers a successfully-handled portal-access trigger (handled=true): the
// trigger consumes the message entirely (no memory append, no agent turn),
// so nothing else in this branch ever looks at ev.Attachments, and a file
// attached to the same message as "manage my accounts" would otherwise
// vanish with zero trace. This decision reports OutcomeRouted, which no
// channel kind's Deliver caller reads Notice from — so the assertion is on
// the envelope actually published to NATS, not on decision state, which
// would pass even if delivery were broken.
func TestDeliverPortalAccess_Handled_AttachmentsNotCarried_UserToldAndLogged(t *testing.T) {
	ch := newChannel("c1")
	active := archivedSession(t, "c1-live-attach", "thread:C1:attach-portal", spiceboxv1alpha1.AgentSessionPhaseRunning, "U1")
	active.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = "user:alice"
	active.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name: "c1", Kind: "fake", Key: "thread:C1:attach-portal",
	}
	p, _, _, nats, _ := newPipeline(t, ch, active)
	// The portal triggerer publishes its own (unrelated) interaction_request
	// through a SEPARATE fake — this test is about the attachment notice,
	// which publishes through the pipeline's own p.NATS (captured above).
	p.PortalAccess = newPortalTriggerer(t, &fakeNATS{})

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:attach-portal",
		MessageText: "manage my accounts",
		Attachments: oneAttachment("F1", "report.pdf", "application/pdf", 2048),
	})
	require.NoError(t, err, "Deliver must not fail the inbound")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "a successfully-handled portal trigger still reports Routed")
	assert.Nil(t, dec.Notice, "OutcomeRouted's Notice is never read by any channel kind — the notice is published directly instead")

	env := findEnvelopeBySubjectSuffix(t, nats, ".out.interaction_request")
	require.Equal(t, channelevents.KindInteractionRequest, env.Kind, "envelope kind")
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal InteractionRequestPayload")
	assert.Equal(t, categories.AttachmentReadFailed, pl.Category, "Category")
	assert.Contains(t, pl.Body, "doesn't carry file attachments", "must tell the user the file did not come along")
}

// TestDeliverPortalAccess_PublishFailure_AttachmentsAlsoNotCarried extends
// the existing publish-failure test: the portalFailureNotice this branch
// already returns must ALSO mention any attachments on the trigger message
// (I4 fix), not just the portal-link failure — both problems share the one
// Notice this transition gets to send.
func TestDeliverPortalAccess_PublishFailure_AttachmentsAlsoNotCarried(t *testing.T) {
	ch := newChannel("c1")
	active := archivedSession(t, "c1-live-attach-fail", "thread:C1:attach-portal-fail", spiceboxv1alpha1.AgentSessionPhaseRunning, "U1")
	active.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = "user:alice"
	active.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name: "c1", Kind: "fake", Key: "thread:C1:attach-portal-fail",
	}
	p, _, _, _, _ := newPipeline(t, ch, active)
	p.PortalAccess = newPortalTriggerer(t, &erroringNATS{err: errors.New("boom")})

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:attach-portal-fail",
		MessageText: "manage my accounts",
		Attachments: oneAttachment("F1", "report.pdf", "application/pdf", 2048),
	})
	require.NoError(t, err, "Deliver must not fail the inbound")
	require.NotNil(t, dec.Notice)
	assert.Contains(t, dec.Notice.Args().Body, "won't carry through", "the portal-failure notice must ALSO name the dropped attachment")
}

// TestPortalAccessPublishesInteractionRequest: portal access rides a published
// interaction_request(portal_access) envelope, delivered through the outbound
// relay's generic "interaction" sub-channel like credential_request — which is
// what keeps it working on every channel kind rather than Slack alone. Mirrors
// TestCredentialRequestWatcher_HappyPath's fakeNATS-based assertion shape.
func TestPortalAccessPublishesInteractionRequest(t *testing.T) {
	nats := &fakeNATS{}
	tr := newPortalTriggerer(t, nats)
	sess := fixturePortalSession()

	handled, err := tr.TryHandle(context.Background(), sess, "manage my accounts")
	require.NoError(t, err, "TryHandle should succeed")
	require.True(t, handled, "trigger phrase should be handled")

	env := findEnvelopeBySubjectSuffix(t, nats, ".out.interaction_request")
	require.Equal(t, channelevents.KindInteractionRequest, env.Kind, "envelope kind")

	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal InteractionRequestPayload")

	assert.Equal(t, categories.PortalAccess, pl.Category, "Category")
	assert.Equal(t, portalTestNS, pl.AgentSessionRef.Namespace, "AgentSessionRef.Namespace")
	assert.Equal(t, portalTestSession, pl.AgentSessionRef.Name, "AgentSessionRef.Name")
	assert.NotEmpty(t, pl.RequestRef, "RequestRef is set")
	assert.NotEmpty(t, pl.Lead, "Lead is set")

	require.Len(t, pl.Actions, 1, "exactly one action")
	action := pl.Actions[0]
	assert.Equal(t, channelevents.ActionKindLink, action.Kind, "action.Kind")
	require.NotEmpty(t, action.URL, "action.URL must be set")
	require.True(t, strings.HasPrefix(action.URL, portalTestExternalURL+"/my/accounts?"),
		"action.URL targets /my/accounts: %s", action.URL)

	require.Equal(t, channelevents.AudienceRequester, pl.Audience.Scope, "Audience.Scope")
	require.NotNil(t, pl.Audience.Requester, "Audience.Requester")
	// Requester carries the natural raw+email started-by identity — honestly
	// raw, never a precomputed canonical stuffed into ExternalID. The
	// consumer derives the canonical via Principal().AllowSynthetic().Canonical(),
	// byte-identical to the canonical this test's link (below) carries
	// directly. See TestPortalAccessPublishesInteractionRequest_NoStartedByEmail_FallsBackToCanonicalSubject
	// for the no-email fallback.
	assert.Equal(t, portalTestKindName, pl.Audience.Requester.Kind.String(), "Audience.Requester.Kind = session's channel kind")
	assert.Equal(t, portalTestStarterExternalID, pl.Audience.Requester.ExternalID.String(), "Audience.Requester.ExternalID = raw started-by external id")
	assert.Equal(t, portalTestStarterEmail, pl.Audience.Requester.Email.String(), "Audience.Requester.Email = verified started-by email")
	assert.Empty(t, pl.Audience.Requester.Subject, "Subject unset: email is present, no synthetic-canonical fallback needed")

	// Decode the link back through the signer and verify its claims.
	u, err := url.Parse(action.URL)
	require.NoError(t, err, "parse action.URL")
	d := u.Query().Get("d")
	sig := u.Query().Get("sig")
	require.NotEmpty(t, d, "?d= present")
	require.NotEmpty(t, sig, "?sig= present")
	decoded, err := tr.LinkSigner.Verify(d + "." + sig)
	require.NoError(t, err, "Verify signed link")
	assert.Equal(t, identity.Subject(portalTestStarter), decoded.Subject, "link Subject = started-by canonical")
	assert.Equal(t, "portal", decoded.Purpose, "link Purpose = portal")
	assert.Empty(t, decoded.SessionRef, "portal link has no SessionRef")
	assert.Empty(t, decoded.RequiredCredentials, "portal link has no RequiredCredentials")
	expectedExp := tr.now().Add(portalAccessTTL).Unix()
	assert.Equal(t, expectedExp, decoded.ExpiresAt, "ExpiresAt = now + 10m")
}

// TestPortalAccessPublishesInteractionRequest_NoStartedByEmail_FallsBackToCanonicalSubject
// covers a started-by user with no verified email on record: TryHandle
// cannot safely re-derive the synthetic kind:teamScope:externalID canonical
// (no StartedByTeamScope annotation preserves the TeamScope the original
// canonical was minted with), so it falls back to the Subject passthrough —
// the exact precomputed canonical (AnnotationStartedByCanonicalID) — keeping
// delivery byte-identical instead of risking a divergent re-derivation.
func TestPortalAccessPublishesInteractionRequest_NoStartedByEmail_FallsBackToCanonicalSubject(t *testing.T) {
	nats := &fakeNATS{}
	tr := newPortalTriggerer(t, nats)
	sess := fixturePortalSession(func(s *spiceboxv1alpha1.AgentSession) {
		delete(s.Annotations, spiceboxv1alpha1.AnnotationStartedByExternalID)
		delete(s.Annotations, spiceboxv1alpha1.AnnotationStartedByEmail)
	})

	handled, err := tr.TryHandle(context.Background(), sess, "manage my accounts")
	require.NoError(t, err, "TryHandle should succeed")
	require.True(t, handled, "trigger phrase should be handled")

	env := findEnvelopeBySubjectSuffix(t, nats, ".out.interaction_request")
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal InteractionRequestPayload")

	require.NotNil(t, pl.Audience.Requester, "Audience.Requester")
	assert.Equal(t, portalTestKindName, pl.Audience.Requester.Kind.String(), "Kind is still populated")
	assert.Empty(t, pl.Audience.Requester.ExternalID, "ExternalID stays empty: no raw id on record")
	assert.Empty(t, pl.Audience.Requester.Email, "Email stays empty: no verified email on record")
	assert.Equal(t, portalTestStarter, pl.Audience.Requester.Subject.String(), "Subject = precomputed canonical, byte-identical fallback")
	require.NoError(t, pl.Validate(), "a Subject-only requester must still be wire-valid")
}

func TestPortalAccessTriggerer_AllPhrasesPublish(t *testing.T) {
	cases := []struct {
		name   string
		phrase string
	}{
		{"manage my accounts (exact)", "manage my accounts"},
		{"link my accounts (exact)", "link my accounts"},
		{"!my/accounts (exact)", "!my/accounts"},
		{"manage my accounts (case-insensitive + whitespace)", "  Manage MY Accounts  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nats := &fakeNATS{}
			tr := newPortalTriggerer(t, nats)
			sess := fixturePortalSession()

			handled, err := tr.TryHandle(context.Background(), sess, tc.phrase)
			require.NoError(t, err, "TryHandle ok")
			assert.True(t, handled, "phrase handled")
			require.Len(t, nats.subjects, 1, "envelope published")
		})
	}
}

// --- non-trigger pass-through -------------------------------------------

func TestPortalAccessTriggerer_NonTriggerPassesThrough(t *testing.T) {
	nats := &fakeNATS{}
	tr := newPortalTriggerer(t, nats)
	sess := fixturePortalSession()

	handled, err := tr.TryHandle(context.Background(), sess, "hello agent, please help")
	require.NoError(t, err)
	assert.False(t, handled, "non-trigger should pass through")
	assert.Empty(t, nats.subjects, "no envelope published")
}

// --- consumed-without-publish edge cases --------------------------------

func TestPortalAccessTriggerer_ConsumedButNoPublish(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PortalAccessTriggerer, *spiceboxv1alpha1.AgentSession)
	}{
		{
			name: "missing started-by annotation: consumed, no publish",
			mutate: func(_ *PortalAccessTriggerer, sess *spiceboxv1alpha1.AgentSession) {
				delete(sess.Annotations, spiceboxv1alpha1.AnnotationStartedByCanonicalID)
			},
		},
		{
			name: "nil signer: consumed, no publish",
			mutate: func(tr *PortalAccessTriggerer, _ *spiceboxv1alpha1.AgentSession) {
				tr.LinkSigner = nil
			},
		},
		{
			name: "empty externalBaseURL: consumed, no publish",
			mutate: func(tr *PortalAccessTriggerer, _ *spiceboxv1alpha1.AgentSession) {
				tr.ExternalBaseURL = func() string { return "" }
			},
		},
		{
			name: "nil NATS publisher: consumed, no publish",
			mutate: func(tr *PortalAccessTriggerer, _ *spiceboxv1alpha1.AgentSession) {
				tr.NATS = nil
			},
		},
		{
			name: "no InputChannel: consumed, no publish",
			mutate: func(_ *PortalAccessTriggerer, sess *spiceboxv1alpha1.AgentSession) {
				sess.Spec.InputChannel = nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nats := &fakeNATS{}
			tr := newPortalTriggerer(t, nats)
			sess := fixturePortalSession()
			tc.mutate(tr, sess)

			handled, err := tr.TryHandle(context.Background(), sess, "manage my accounts")
			require.NoError(t, err, "TryHandle ok despite skipped publish")
			assert.True(t, handled, "trigger consumed even on no-publish path")
			assert.Empty(t, nats.subjects, "no envelope published")
		})
	}
}

// --- error paths --------------------------------------------------------

// TestPortalAccessTriggerer_PublishError verifies that a NATS publish
// failure surfaces to the caller (TryHandle returns handled=true, err!=nil)
// so Deliver can tell the user instead of stranding them — see
// TestDeliverPortalAccess_PublishFailure_TellsTheUserInsteadOfStrandingThem
// for the Deliver-level assertion. Replaces the pre-Task-6 resolver-error
// test now that there is no sub-channel resolver in the publish path.
func TestPortalAccessTriggerer_PublishError(t *testing.T) {
	tr := newPortalTriggerer(t, &erroringNATS{err: errors.New("boom")})
	sess := fixturePortalSession()

	handled, err := tr.TryHandle(context.Background(), sess, "manage my accounts")
	require.True(t, handled, "trigger still consumed on publish error")
	require.Error(t, err, "TryHandle surfaces publish error to caller")
}
