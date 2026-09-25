// pkg/channels/channelsd/pipeline/credential_update_agentowned_test.go
//
// The AgentIdentity route of CredentialUpdateWatcher: where a card goes when
// the dead credential is the AGENT's own shared secret rather than a person's.
//
// The invariant every test here defends: status.interactionRef is stamped ONLY
// when a card was really delivered, so the operator's Expired Reason can keep
// saying "never delivered" rather than "nobody updated it in time".
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

const (
	curAgentIdentityName = "demo-agent-identity"
	monitoringChannelNS  = testNS
	monitoringChannel    = "ch-monitoring"
	// monitoringSecretName backs every monitoring Channel fixture here.
	// Channel.spec.credentialsRef.secretName is mandatory in the CRD, and the
	// pre-check reads the Secret whether or not the kind declares required
	// keys -- because the relay does. A fixture without it would be testing a
	// Channel no cluster can produce.
	monitoringSecretName = "ch-monitoring-creds"
)

// fakeCredentialUpdateAuthz is a CredentialUpdateAuthz whose answer is fixed
// per test. It records the (ns, name, canonical) it was asked about so a test
// can prove the check targets the AGENT IDENTITY that owns the dead credential
// -- not the session, and not the identity the request happens to live next to.
type fakeCredentialUpdateAuthz struct {
	allow bool
	err   error

	calls []credUpdateAuthzCall
}

type credUpdateAuthzCall struct {
	ns, name  string
	canonical identity.CanonicalUserID
}

func (f *fakeCredentialUpdateAuthz) CheckAgentIdentityUpdateCredential(
	_ context.Context, ns, name string, canonicalID identity.CanonicalUserID,
) (bool, error) {
	f.calls = append(f.calls, credUpdateAuthzCall{ns: ns, name: name, canonical: canonicalID})
	if f.err != nil {
		return false, f.err
	}
	return f.allow, nil
}

// fixtureAgentOwnedCUR is fixtureCUR with the resolved credential re-pointed at
// an AgentIdentity -- i.e. the agent's own shared secret. Everything else
// (session, reason, why, tool) is byte-identical to the user-owned fixture, so
// any assertion that differs between the two is genuinely about the ROUTE and
// not about incidental fixture drift.
func fixtureAgentOwnedCUR(mutate ...func(*spiceboxv1alpha1.CredentialUpdateRequest)) *spiceboxv1alpha1.CredentialUpdateRequest {
	all := append([]func(*spiceboxv1alpha1.CredentialUpdateRequest){
		func(c *spiceboxv1alpha1.CredentialUpdateRequest) {
			c.Status.ResolvedCredential.IdentityKind = "AgentIdentity"
			c.Status.ResolvedCredential.Name = curAgentIdentityName
			c.Spec.RequestedBy = identity.Subject(testStarter)
		},
	}, mutate...)
	return fixtureCUR(all...)
}

// fixtureMonitoringChannel is a role=monitoring Channel that is genuinely
// DELIVERABLE: the fake kind reports SupportsMonitoring()==true, its
// ValidateSpec accepts this spec, and its credentials Secret is readable. The
// watcher's pre-check asks all of those, so a Channel that merely carried the
// role would NOT make this the reachable surface these tests assume.
//
// It must be paired with fixtureMonitoringSecret(); a Channel alone is NOT a
// recipient.
func fixtureMonitoringChannel() *spiceboxv1alpha1.Channel {
	return monitoringChannelOfKind(testKindName, func(*spiceboxv1alpha1.Channel) {})
}

// fixtureMonitoringSecret is the credentials Secret fixtureMonitoringChannel
// points at. Its DATA is empty on purpose: the fake kind declares no required
// keys, so what makes the Channel deliverable is that the Secret EXISTS -- the
// relay Gets it unconditionally before it can build any sender.
func fixtureMonitoringSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: monitoringSecretName, Namespace: monitoringChannelNS},
	}
}

// monitoringChannelOfKind builds a role=monitoring Channel of an arbitrary
// kind so the deliverability tests can produce each configuration an operator
// can really create.
func monitoringChannelOfKind(kind string, mutate func(*spiceboxv1alpha1.Channel)) *spiceboxv1alpha1.Channel {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: monitoringChannel, Namespace: monitoringChannelNS},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           kind,
			Role:           spiceboxv1alpha1.ChannelRoleMonitoring,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: monitoringSecretName},
		},
	}
	mutate(ch)
	return ch
}

// newAgentOwnedWatcher wires newCredentialUpdateWatcher plus an Authz whose
// verdict the test controls.
func newAgentOwnedWatcher(t *testing.T, authz *fakeCredentialUpdateAuthz, objs ...client.Object) (
	client.Client, *CredentialUpdateWatcher, *capturingPublisher,
) {
	t.Helper()
	c, w, pub := newCredentialUpdateWatcher(t, objs...)
	if authz != nil {
		w.Authz = authz
	}
	return c, w, pub
}

// --- (1) Monitoring-channel delivery -------------------------------------

// TestReconcileOne_AgentOwnedPublishesToMonitoringChannel is the core routing
// assertion: an AgentIdentity credential reaches the role=monitoring Channel.
//
// Mutation sensitivity: delete the publishAgentOwnedMonitoring call (or make
// publishAgentOwnedCard fall through to the user-owned path) and findMonitoring
// fails the test outright.
func TestReconcileOne_AgentOwnedPublishesToMonitoringChannel(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: false},
		sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	ev := pub.findMonitoring(t)
	assert.Equal(t, "AgentIdentity", ev.Source.Kind, "the event must be sourced to the identity whose credential died")
	assert.Equal(t, curAgentIdentityName, ev.Source.Name)
	assert.Equal(t, testNS, ev.Source.Namespace)
	assert.Equal(t, channelevents.MonitoringLevelError, ev.Level)
	assert.Equal(t, "credential", ev.Category)
	assert.NotEmpty(t, ev.Hint, "the event must carry the actionable link, or an admin sees a report with nothing to do")

	got := getCURFrom(t, c)
	assert.Equal(t, metav1.ConditionTrue, conditionStatusOf(t, got, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered),
		"a delivered card must record CardDelivered=True")
	assert.NotEmpty(t, got.Status.InteractionRef, "a card that WAS delivered must stamp InteractionRef")
}

// TestReconcileOne_AgentOwnedMonitoringLinkIsNotSubjectBound pins the one
// property that separates the broadcast link from the in-thread one. A
// monitoring channel has no single addressee, so a subject-bound link would be
// inert for everyone reading it; the click is authorized at identityd instead.
func TestReconcileOne_AgentOwnedMonitoringLinkIsNotSubjectBound(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	_, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: false},
		sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	linkURL := linkFromHint(t, pub.findMonitoring(t).Hint)
	payload := decodeLinkPayload(t, w.LinkSigner, linkURL)
	assert.True(t, payload.Subject.Empty(),
		"the broadcast link must carry NO subject -- binding it to one person makes it inert for every other admin reading the channel")
	assert.Equal(t, sess.Namespace+"/"+sess.Name, payload.SessionRef)
	assert.Equal(t, []string{curCredName}, payload.RequiredCredentials)
}

// --- (2) Regression guard: user-owned credentials are untouched ----------

// TestReconcileOne_UserOwnedCredentialDMsUserAndNeverPostsToMonitoring: a
// UserIdentity/SessionUserIdentity credential belongs to ONE person and must
// go to them alone. A monitoring broadcast of a person's own dead credential
// puts their failure detail in front of every platform admin.
//
// Mutation sensitivity: route user-owned credentials through
// publishAgentOwnedCard and countMonitoring goes to 1.
func TestReconcileOne_UserOwnedCredentialDMsUserAndNeverPostsToMonitoring(t *testing.T) {
	for _, kind := range []string{"UserIdentity", "SessionUserIdentity"} {
		t.Run(kind+": DMs its own user, publishes no MonitoringEvent", func(t *testing.T) {
			sess := fixtureSession(t)
			cur := fixtureCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) {
				c.Status.ResolvedCredential.IdentityKind = kind
			})
			// A monitoring Channel EXISTS -- so a regression that broadcasts
			// would succeed rather than fall into the no-recipient branch.
			_, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: true},
				sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

			require.NoError(t, w.ReconcileOne(context.Background(), cur))

			assert.Zero(t, pub.countMonitoring(),
				"a person's own credential must never be broadcast to the monitoring channel")

			req := pub.findInteractionRequest(t)
			require.NotNil(t, req.Audience.Requester)
			assert.Equal(t, identity.RawExternalID(testStarterExternalID), req.Audience.Requester.ExternalID,
				"the card must still DM the credential's own user")
			for _, f := range req.Fields {
				assert.NotEqual(t, "Affects", f.Label,
					"a personal credential has no shared blast radius; the field belongs to the agent-owned card only")
			}
		})
	}
}

// --- (3) The in-thread admin card ----------------------------------------

// TestReconcileOne_InThreadAdminCardWhenTurnAuthorPasses proves the second
// surface: when the person who asked can personally fix it, the card is
// rendered in the thread too, addressed to them and bound to their subject.
func TestReconcileOne_InThreadAdminCardWhenTurnAuthorPasses(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	authz := &fakeCredentialUpdateAuthz{allow: true}
	_, w, pub := newAgentOwnedWatcher(t, authz, sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	require.Len(t, authz.calls, 1, "exactly one permission check, on the turn's author")
	assert.Equal(t, testNS, authz.calls[0].ns)
	assert.Equal(t, curAgentIdentityName, authz.calls[0].name,
		"the check must target the AgentIdentity that owns the dead credential")
	assert.Equal(t, identity.CanonicalFromTrusted("alice", "test fixture"), authz.calls[0].canonical)

	req := pub.findInteractionRequest(t)
	require.NotNil(t, req.Audience.Requester)
	assert.Equal(t, sess.Spec.InputChannel.Kind, string(req.Audience.Requester.Kind),
		"a Kind-less requester is dropped by the channel kind before it is ever rendered")
	assert.True(t, req.Audience.Requester.HasIdentity())

	require.Len(t, req.Actions, 1, "exactly one action, platform-labelled")
	payload := decodeLinkPayload(t, w.LinkSigner, req.Actions[0].URL)
	assert.Equal(t, identity.Subject(testStarter), payload.Subject,
		"the in-thread admin link is subject-bound to the author who passed the check")
}

// TestReconcileOne_NoInThreadCardWhenTurnAuthorFailsTheCheck is the negative
// half: an ordinary starter must never be handed a button that would write
// their pasted value into their OWN UserIdentity while the agent's shared
// credential stays broken.
func TestReconcileOne_NoInThreadCardWhenTurnAuthorFailsTheCheck(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	_, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: false},
		sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	assert.Empty(t, pub.interactionRequests(t),
		"a starter without update_credential must get no card -- the monitoring channel is the only surface")
	assert.Equal(t, 1, pub.countMonitoring(), "the monitoring broadcast still happens")
}

// TestReconcileOne_InThreadAdminCardFailsClosed covers every way the check can
// fail to say a clear "yes". None may render the card.
func TestReconcileOne_InThreadAdminCardFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		authz  *fakeCredentialUpdateAuthz
		mutate func(*spiceboxv1alpha1.CredentialUpdateRequest)
	}{
		{
			name:  "authorization client not wired: suppressed, not assumed",
			authz: nil,
		},
		{
			name:  "check returns an error: suppressed, not assumed",
			authz: &fakeCredentialUpdateAuthz{err: errors.New("spicedb unavailable")},
		},
		{
			name:   "request records no turn author: nobody to address",
			authz:  &fakeCredentialUpdateAuthz{allow: true},
			mutate: func(c *spiceboxv1alpha1.CredentialUpdateRequest) { c.Spec.RequestedBy = "" },
		},
		{
			name:   "turn author is not a user subject: refused, not trimmed",
			authz:  &fakeCredentialUpdateAuthz{allow: true},
			mutate: func(c *spiceboxv1alpha1.CredentialUpdateRequest) { c.Spec.RequestedBy = "group:platform-admins" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := fixtureSession(t)
			var mutations []func(*spiceboxv1alpha1.CredentialUpdateRequest)
			if tc.mutate != nil {
				mutations = append(mutations, tc.mutate)
			}
			cur := fixtureAgentOwnedCUR(mutations...)
			_, w, pub := newAgentOwnedWatcher(t, tc.authz, sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

			require.NoError(t, w.ReconcileOne(context.Background(), cur))

			assert.Empty(t, pub.interactionRequests(t), "no in-thread card may be rendered without a clear yes")
			assert.Equal(t, 1, pub.countMonitoring(), "the monitoring broadcast is unaffected by the in-thread decision")
		})
	}
}

// TestReconcileOne_InThreadAdminCardAloneCountsAsDelivery: a cluster with no
// monitoring Channel but an admin in the thread HAS reached a human, so the
// request must record delivery rather than fall into the no-recipient branch.
func TestReconcileOne_InThreadAdminCardAloneCountsAsDelivery(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: true}, sess, cur)

	require.NoError(t, w.ReconcileOne(context.Background(), cur),
		"an admin in the thread is a reachable recipient even with no monitoring Channel")

	assert.Zero(t, pub.countMonitoring(), "there is no monitoring Channel to broadcast to")
	assert.Len(t, pub.interactionRequests(t), 1)

	got := getCURFrom(t, c)
	assert.NotEmpty(t, got.Status.InteractionRef, "a card WAS delivered, so InteractionRef must be stamped")
}

// --- (4) Blast radius ----------------------------------------------------

// TestReconcileOne_AgentOwnedCardNamesBlastRadius pins requirement 4: both
// surfaces must name the identity AND the credential AND say the replacement
// is shared. Naming them without the shared-ness reads exactly like the
// user-owned card, and an admin can reasonably think they are re-linking their
// own account rather than rotating a token every session uses.
func TestReconcileOne_AgentOwnedCardNamesBlastRadius(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	_, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: true},
		sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	req := pub.findInteractionRequest(t)
	var affects string
	fieldValues := map[string]string{}
	for _, f := range req.Fields {
		fieldValues[f.Label] = f.Value
	}
	affects = fieldValues["Affects"]
	require.NotEmpty(t, affects, "the agent-owned card must carry a blast-radius row")
	assert.Contains(t, affects, curCredName, "the blast-radius row must name the credential")
	assert.Contains(t, affects, curAgentIdentityName, "the blast-radius row must name the identity")
	assert.Contains(t, strings.ToLower(affects), "every session",
		"the row must say the replacement is shared, not just name things")
	assert.Contains(t, fieldValues["Identity"], curAgentIdentityName)
	assert.Equal(t, curCredName, fieldValues["Credential"])

	summary := pub.findMonitoring(t).Summary
	assert.Contains(t, summary, curAgentIdentityName, "the monitoring event must name the identity")
	assert.Contains(t, summary, curCredName, "the monitoring event must name the credential")
	assert.Contains(t, strings.ToLower(summary), "every session",
		"the monitoring event must carry the same blast radius as the card")
}

// TestReconcileOne_AgentOwnedKeepsVerdictPlatformAuthoredAndWhyAttributed
// carries slice 1's card-content rules onto the new surfaces: the verdict line
// is the platform's (sanitized, because for a VerifyRejected determination it
// embeds the provider's raw response), and the agent's own words never appear
// outside their attributed block.
func TestReconcileOne_AgentOwnedKeepsVerdictPlatformAuthoredAndWhyAttributed(t *testing.T) {
	const dirty = "Provider says:\n\x1b[31mFATAL\x1b[0m\r\ntoken revoked"
	const why = "my push kept failing so I assumed the token expired"

	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) {
		c.Status.Reason = dirty
		c.Spec.Why = why
	})
	_, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: true},
		sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	req := pub.findInteractionRequest(t)
	assert.Equal(t, sanitizeCardText(dirty), req.Lead,
		"the verdict line stays the platform's, sanitized -- it can embed the provider's raw response")
	assert.NotContains(t, req.Lead, "\n", "a sanitized verdict can never occupy more than one line")
	assert.Equal(t, attributionPrefix+why, req.Body,
		"the agent's why stays inside its attributed block")

	summary := pub.findMonitoring(t).Summary
	assert.Contains(t, summary, sanitizeCardText(dirty))
	assert.Contains(t, summary, attributionPrefix+why,
		"the monitoring event must attribute the agent's words the same way the card does")
	assert.NotContains(t, summary, "\x1b", "control characters must not reach the monitoring surface")
}

// TestReconcileOne_AgentOwnedOmitsAttributionWhenWhyIsEmpty: no dangling "The
// agent said:" on either surface.
func TestReconcileOne_AgentOwnedOmitsAttributionWhenWhyIsEmpty(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR(func(c *spiceboxv1alpha1.CredentialUpdateRequest) { c.Spec.Why = "   " })
	_, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: true},
		sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	assert.Empty(t, pub.findInteractionRequest(t).Body)
	assert.NotContains(t, pub.findMonitoring(t).Summary, attributionPrefix)
}

// --- (5) No reachable recipient is LOUD ----------------------------------

// TestReconcileOne_NoRecipientIsLoudAndLeavesInteractionRefEmpty: when nobody
// can be shown the card, InteractionRef stays EMPTY (so the operator's Expired
// Reason keeps saying "never delivered") AND the condition is surfaced AND an
// error is returned so the next tick retries. A silent nil return here is the
// silent-hang class.
//
// Mutation sensitivity: change handleNoRecipient's `return fmt.Errorf(...)` to
// `return nil` and the require.Error fails; drop the condition patch and the
// CardDelivered assertion fails; stamp InteractionRef and the emptiness
// assertion fails.
func TestReconcileOne_NoRecipientIsLoudAndLeavesInteractionRefEmpty(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	// No monitoring Channel object, and the author fails the check.
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: false}, sess, cur)

	err := w.ReconcileOne(context.Background(), cur)
	require.Error(t, err, "a request nobody can be shown must fail loudly, not park silently")
	assert.Contains(t, err.Error(), curAgentIdentityName, "the error must name what could not be delivered")

	assert.Zero(t, pub.countMonitoring())
	assert.Empty(t, pub.interactionRequests(t))

	notes := pub.envelopesOfKind(channelevents.KindNotification)
	assert.Len(t, notes, 1, "the humans in the thread must be told the agent is blocked on an admin")

	got := getCURFrom(t, c)
	assert.Empty(t, got.Status.InteractionRef,
		"nothing was delivered, so InteractionRef must stay empty or the operator's Expired Reason would lie")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
	require.NotNil(t, cond, "the no-recipient state must be visible on the CR")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonCredentialUpdateNoRecipient, cond.Reason)
	assert.Contains(t, cond.Message, "role=monitoring",
		"the condition must name WHICH surface was missing -- an operator reading it should not have to source-dive")
	assert.Contains(t, cond.Message, "update_credential",
		"...and that the other surface needs a permission the author lacks")

	_, tracked := w.alreadyPublished(cur.UID)
	assert.False(t, tracked, "nothing was published, so the dedup map must not suppress a later real publish")
}

// TestReconcileOne_NoRecipientNotifiesOnlyOnFirstTransition: reconcileAll
// re-dispatches this same CR every 5s for the whole park, so an
// unconditional notification would be ~360 writes per affected session. The
// CardDelivered=False/NoRecipient condition is the dedup key.
func TestReconcileOne_NoRecipientNotifiesOnlyOnFirstTransition(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: false}, sess, cur)

	require.Error(t, w.ReconcileOne(context.Background(), cur))
	first := len(pub.envelopesOfKind(channelevents.KindNotification))
	require.Equal(t, 1, first)

	got := getCURFrom(t, c)
	require.Error(t, w.ReconcileOne(context.Background(), got), "still fails closed on the second tick")
	assert.Equal(t, first, len(pub.envelopesOfKind(channelevents.KindNotification)),
		"a repeated identical failure must not re-notify")
}

// TestReconcileOne_NoRecipientRecoversOnceAMonitoringChannelExists proves the
// retry is not decorative: the returned error is what brings the request back
// next tick, and the state it left behind (no InteractionRef, no dedup entry)
// is what lets that retry actually publish.
func TestReconcileOne_NoRecipientRecoversOnceAMonitoringChannelExists(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: false}, sess, cur)

	require.Error(t, w.ReconcileOne(context.Background(), cur))
	require.Zero(t, pub.countMonitoring())

	require.NoError(t, c.Create(context.Background(), fixtureMonitoringSecret()))
	require.NoError(t, c.Create(context.Background(), fixtureMonitoringChannel()))

	got := getCURFrom(t, c)
	require.NoError(t, w.ReconcileOne(context.Background(), got),
		"the very next tick after an operator configures a monitoring Channel must succeed")
	assert.Equal(t, 1, pub.countMonitoring())
	assert.NotEmpty(t, getCURFrom(t, c).Status.InteractionRef)
}

// --- (5b) EXISTENCE is not DELIVERABILITY --------------------------------

// TestReconcileOne_UndeliverableMonitoringChannelIsNotARecipient: a
// role=monitoring Channel whose KIND cannot deliver a MonitoringEvent at all
// is silently dropped by the relay (internal/cmd/channelsd/monitoring_relay.go). A
// pre-check that asked only "does a Channel with role=monitoring exist" would
// count that as delivered, stamp CardDelivered=True + InteractionRef, and
// DISARM the "nobody was ever asked" branch for a card literally nobody
// received -- the exact falsehood that condition exists to prevent.
//
// `local` and `bento` both return SupportsMonitoring()==false; `slack` returns
// true but its ValidateSpec requires outputDefaults.channelId for a monitoring
// Channel, and its RequiredSecretKeys demands a bot token. Each row is a
// configuration an operator can really produce.
//
// Mutation sensitivity: revert monitoringRecipientExists to a bare
// role==monitoring existence check and every row flips to "delivered".
func TestReconcileOne_UndeliverableMonitoringChannelIsNotARecipient(t *testing.T) {
	cases := []struct {
		name    string
		channel *spiceboxv1alpha1.Channel
		want    string // substring the CR condition must carry
	}{
		{
			name:    "kind has no MonitoringSender (local): not a recipient",
			channel: monitoringChannelOfKind("local", func(ch *spiceboxv1alpha1.Channel) {}),
			want:    "does not deliver monitoring events",
		},
		{
			name:    "kind has no MonitoringSender (bento): not a recipient",
			channel: monitoringChannelOfKind("bento", func(ch *spiceboxv1alpha1.Channel) {}),
			want:    "does not deliver monitoring events",
		},
		{
			name:    "kind is not registered in this binary: not a recipient",
			channel: monitoringChannelOfKind("carrier-pigeon", func(ch *spiceboxv1alpha1.Channel) {}),
			want:    "not registered",
		},
		{
			name: "slack monitoring Channel with no outputDefaults.channelId: not a recipient",
			channel: monitoringChannelOfKind("slack", func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.Slack = &spiceboxv1alpha1.SlackChannelConfig{}
			}),
			want: "spec is invalid for its kind",
		},
		{
			// The trap row. The fake kind declares NO required Secret keys, so a
			// pre-check that skipped the Secret read when RequiredSecretKeys was
			// empty would call this deliverable -- while
			// the relay, which resolves every monitoring Channel through
			// resolve.ForChannel, Gets the Secret unconditionally and drops the
			// Channel. Result: CardDelivered=True and InteractionRef stamped for
			// a card nobody received, disarming the operator's never-delivered
			// branch. Reachable in any cluster whose monitoring Channel names a
			// Secret that was deleted or never created.
			name:    "kind needs no Secret keys but its credentials Secret is missing: STILL not a recipient",
			channel: monitoringChannelOfKind(testKindName, func(*spiceboxv1alpha1.Channel) {}),
			want:    "is unreadable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := fixtureSession(t)
			cur := fixtureAgentOwnedCUR()
			// The author fails the check too, so the monitoring surface is the
			// ONLY thing that could have delivered.
			c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: false}, sess, cur, tc.channel)

			err := w.ReconcileOne(context.Background(), cur)
			require.Error(t, err, "an undeliverable monitoring Channel is not a recipient; this must take the loud path")

			assert.Zero(t, pub.countMonitoring(),
				"nothing may be broadcast at a Channel that cannot receive it")

			got := getCURFrom(t, c)
			assert.Empty(t, got.Status.InteractionRef,
				"the card reached NOBODY, so InteractionRef must stay empty -- stamping it disarms the operator's never-delivered branch")
			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, spiceboxv1alpha1.ReasonCredentialUpdateNoRecipient, cond.Reason)
			assert.Contains(t, cond.Message, tc.want,
				"the condition must name WHY this Channel could not receive it, not just that nothing was delivered")
		})
	}
}

// TestReconcileOne_MonitoringChannelMissingItsSecretKeyIsNotARecipient covers
// the third relay failure: the kind CAN do monitoring and the spec is valid,
// but the credentials Secret lacks the key the kind declares it needs, so
// slack's SendMonitoring errors at send time and the relay logs-and-drops.
func TestReconcileOne_MonitoringChannelMissingItsSecretKeyIsNotARecipient(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	ch := monitoringChannelOfKind("slack", func(ch *spiceboxv1alpha1.Channel) {
		ch.Spec.Slack = &spiceboxv1alpha1.SlackChannelConfig{
			OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C_MONITORING"},
		}
		ch.Spec.CredentialsRef = spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-monitoring-creds"}
	})
	emptySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-monitoring-creds", Namespace: monitoringChannelNS},
		Data:       map[string][]byte{},
	}
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: false}, sess, cur, ch, emptySecret)

	require.Error(t, w.ReconcileOne(context.Background(), cur),
		"a monitoring Channel whose Secret cannot authenticate its sender is not a recipient")
	assert.Zero(t, pub.countMonitoring())
	assert.Empty(t, getCURFrom(t, c).Status.InteractionRef)

	cond := findCondition(getCURFrom(t, c).Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "bot-token", "the condition must name the missing Secret key")
}

// TestReconcileOne_DeliverableMonitoringChannelStillDelivers is the positive
// control for the two tests above: the pre-check must not have become so
// strict that a genuinely-usable Channel is rejected.
func TestReconcileOne_DeliverableMonitoringChannelStillDelivers(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	ch := monitoringChannelOfKind("slack", func(ch *spiceboxv1alpha1.Channel) {
		ch.Spec.Slack = &spiceboxv1alpha1.SlackChannelConfig{
			OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C_MONITORING"},
		}
		ch.Spec.CredentialsRef = spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-monitoring-creds"}
	})
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-monitoring-creds", Namespace: monitoringChannelNS},
		Data:       map[string][]byte{"bot-token": []byte("xoxb-not-a-real-token")},
	}
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: false}, sess, cur, ch, sec)

	require.NoError(t, w.ReconcileOne(context.Background(), cur))
	assert.Equal(t, 1, pub.countMonitoring())
	assert.NotEmpty(t, getCURFrom(t, c).Status.InteractionRef)
}

// TestReconcileOne_NoRecipientNamesTheACTUALCause: each of the four
// fail-closed branches of turnAuthorMayUpdateCredential must name its own
// cause on the CR condition -- the durable, kubectl-visible surface. Sharing
// one fixed sentence sends an operator whose channelsd lost its SpiceDB client
// off to grant a permission that was never the problem.
func TestReconcileOne_NoRecipientNamesTheACTUALCause(t *testing.T) {
	cases := []struct {
		name    string
		authz   *fakeCredentialUpdateAuthz
		mutate  func(*spiceboxv1alpha1.CredentialUpdateRequest)
		want    string
		mustNot string
	}{
		{
			name:    "authorization client not wired: names the wiring bug, not a missing grant",
			authz:   nil,
			want:    "no authorization client",
			mustNot: "does not hold agentidentity#update_credential",
		},
		{
			name:    "check errored: names the service fault, not a denial",
			authz:   &fakeCredentialUpdateAuthz{err: errors.New("spicedb unavailable")},
			want:    "FAILED and was refused fail-closed",
			mustNot: "does not hold agentidentity#update_credential",
		},
		{
			name:    "no turn author recorded: names the missing author",
			authz:   &fakeCredentialUpdateAuthz{allow: true},
			mutate:  func(c *spiceboxv1alpha1.CredentialUpdateRequest) { c.Spec.RequestedBy = "" },
			want:    "records no turn author",
			mustNot: "does not hold agentidentity#update_credential",
		},
		{
			name:    "genuine denial: says exactly that",
			authz:   &fakeCredentialUpdateAuthz{allow: false},
			want:    "does not hold agentidentity#update_credential",
			mustNot: "authorization client",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := fixtureSession(t)
			var mutations []func(*spiceboxv1alpha1.CredentialUpdateRequest)
			if tc.mutate != nil {
				mutations = append(mutations, tc.mutate)
			}
			cur := fixtureAgentOwnedCUR(mutations...)
			c, w, _ := newAgentOwnedWatcher(t, tc.authz, sess, cur)

			require.Error(t, w.ReconcileOne(context.Background(), cur))

			cond := findCondition(getCURFrom(t, c).Status.Conditions,
				spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
			require.NotNil(t, cond)
			assert.Contains(t, cond.Message, tc.want,
				"the durable condition must name the ACTUAL cause an operator has to act on")
			assert.NotContains(t, cond.Message, tc.mustNot,
				"...and must not point them at a cause that is not the cause")
		})
	}
}

// --- (6) The shared dedup/record machinery still applies -----------------

// TestReconcileOne_AgentOwnedPublishesAtMostOnce: the agent-owned route reuses
// the SAME dedup + delivery-record machinery, so a second pass over an
// already-published request must publish nothing further -- on either surface.
func TestReconcileOne_AgentOwnedPublishesAtMostOnce(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: true},
		sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), cur))
	require.Equal(t, 1, pub.countMonitoring())
	require.Len(t, pub.interactionRequests(t), 1)

	got := getCURFrom(t, c)
	require.NoError(t, w.ReconcileOne(context.Background(), got))
	assert.Equal(t, 1, pub.countMonitoring(), "the persisted InteractionRef must suppress a second broadcast")
	assert.Len(t, pub.interactionRequests(t), 1, "and a second in-thread card")
}

// TestReconcileOne_AgentOwnedSurfacesShareOneRequestRef: the two surfaces are
// one interaction, so they must carry the SAME requestRef -- that value is
// what recordDelivery persists, and a decision arriving from either surface
// has to resolve the same request.
func TestReconcileOne_AgentOwnedSurfacesShareOneRequestRef(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: true},
		sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), cur))

	req := pub.findInteractionRequest(t)
	assert.Equal(t, req.RequestRef, getCURFrom(t, c).Status.InteractionRef,
		"the persisted InteractionRef must be the requestRef the card carries")
}

// TestReconcileOne_AgentOwnedExternalURLUnconfiguredFailsClosed: the
// agent-owned route must not emit a hostless button either.
func TestReconcileOne_AgentOwnedExternalURLUnconfiguredFailsClosed(t *testing.T) {
	sess := fixtureSession(t)
	cur := fixtureAgentOwnedCUR()
	c, w, pub := newAgentOwnedWatcher(t, &fakeCredentialUpdateAuthz{allow: true},
		sess, cur, fixtureMonitoringChannel(), fixtureMonitoringSecret())
	w.ExternalBaseURL = func() string { return "" }

	require.Error(t, w.ReconcileOne(context.Background(), cur))
	assert.Zero(t, pub.countMonitoring(), "no broadcast may carry a broken link")
	assert.Empty(t, pub.interactionRequests(t))
	assert.Empty(t, getCURFrom(t, c).Status.InteractionRef)
}

// --- helpers -------------------------------------------------------------

// conditionStatusOf returns the status of condType on cur, failing the test
// when the condition is absent.
func conditionStatusOf(t *testing.T, cur *spiceboxv1alpha1.CredentialUpdateRequest, condType string) metav1.ConditionStatus {
	t.Helper()
	cond := findCondition(cur.Status.Conditions, condType)
	require.NotNil(t, cond, "condition %q must be present", condType)
	return cond.Status
}

// linkFromHint extracts the URL the monitoring event's Hint carries. The Hint
// is "<label>: <url>"; this asserts the URL half is actually there rather than
// silently returning "" and letting a downstream Verify produce a confusing
// failure.
func linkFromHint(t *testing.T, hint string) string {
	t.Helper()
	idx := strings.Index(hint, "http")
	require.GreaterOrEqual(t, idx, 0, fmt.Sprintf("monitoring hint %q must embed an absolute link", hint))
	return hint[idx:]
}
