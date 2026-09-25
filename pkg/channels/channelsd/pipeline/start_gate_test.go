package pipeline

// start_gate_test.go covers the session-START gate: on a channel whose kind
// attributes org membership, a NEW session may be created freely only by an
// org member. A guest (or an identity the kind failed to stamp — fail-closed)
// must hold agentclass#start_session; otherwise the session is created PARKED
// for platform-admin approval: no started_by tuple, no interact-policy
// participant, a durable PendingRequesters entry, and a start_approval
// interaction_request addressed to the platform admins.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// newOrgScopedChannel is newChannel with spec.fake.orgScoped=true, so the
// fake kind attributes org membership and the start gate applies.
func newOrgScopedChannel(name string) *spiceboxv1alpha1.Channel {
	ch := newChannel(name)
	ch.Spec.Fake.OrgScoped = true
	return ch
}

// guestIdentity is a stamped non-member identity on the fake kind.
func guestIdentity(id string) channelkinds.ExternalIdentity {
	return channelkinds.ExternalIdentity{
		Kind: "fake", ExternalID: identity.RawExternalID(id), Email: identity.Email(strings.ToLower(id) + "@example.com"),
		OrgMembership: channelkinds.OrgMembershipGuest,
	}
}

// startSessionRefused configures the fake authz to answer false to the
// agentclass#start_session check while recording what was asked.
func startSessionRefused(az *fakeAuthz) *[][3]string {
	var calls [][3]string
	az.checkOwnerFn = func(resType, resID, canonicalID string) (bool, error) {
		calls = append(calls, [3]string{resType, resID, canonicalID})
		return false, nil
	}
	return &calls
}

// publishedInteractionRequests decodes every published .out interaction_request.
func publishedInteractionRequests(t *testing.T, nats *fakeNATS) []channelevents.InteractionRequestPayload {
	t.Helper()
	var out []channelevents.InteractionRequestPayload
	for i, subj := range nats.subjects {
		if !strings.HasSuffix(subj, ".out."+string(channelevents.KindInteractionRequest)) {
			continue
		}
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(nats.payloads[i], &env), "decode envelope")
		var pl channelevents.InteractionRequestPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "decode interaction request")
		out = append(out, pl)
	}
	return out
}

// TestStartGate_GuestParkedForAdminApproval: a stamped guest without
// agentclass#start_session gets a PARKED session — created, but with no
// started_by tuple and no interact-policy participant — plus a durable
// pending entry and a start_approval card addressed to the platform admins.
func TestStartGate_GuestParkedForAdminApproval(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	p, az, _, nats, cli := newPipeline(t, ch)
	calls := startSessionRefused(az)
	az.lookupSubjectsResult = []string{"YWRtaW5AZXhhbXBsZS5jb20"} // one addressable platform admin

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: guestIdentity("U_GUEST"),
		ChannelKey:  "thread:C1:1",
		MessageText: "hi, please run the report",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "outcome: held for approval, not routed")

	// The start_session check was asked of the session's class for the guest.
	require.NotEmpty(t, *calls, "agentclass#start_session must be checked")
	assert.Equal(t, "agentclass", (*calls)[0][0], "check resource type")
	assert.Equal(t, "default/ac1", (*calls)[0][1], "check resource id")

	// The session exists, parked: no authz standing written.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions), "List sessions")
	require.Len(t, sessions.Items, 1, "parked session must be created")
	sess := sessions.Items[0]
	assert.Equal(t, 0, az.startedByCalls, "no started_by tuple while parked")
	assert.Equal(t, 0, az.participantCalls, "no interact-policy participant while parked")
	require.True(t, spiceboxv1alpha1.StartApprovalPending(&sess), "start-approval annotation must be stamped on the Create")

	// Durable pending entry, tagged with the start_approval category so the
	// decision pipe's category witness cannot mistake it for a join request.
	require.Len(t, sess.Status.PendingRequesters, 1, "pending requester recorded")
	pr := sess.Status.PendingRequesters[0]
	assert.Equal(t, categories.StartApproval, pr.Category, "pending entry category")
	assert.Equal(t, "U_GUEST", pr.ExternalID, "pending entry requester")
	assert.Equal(t, spiceboxv1alpha1.StartApprovalRequestRef(&sess), pr.RequestRef,
		"annotation and pending entry must share one requestRef")
	cond := conditions.Find(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStartApprovalPending)
	require.NotNil(t, cond, "StartApprovalPending condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	// The card went to the platform admins, not to the guest or a session owner.
	reqs := publishedInteractionRequests(t, nats)
	require.Len(t, reqs, 1, "one start_approval interaction request")
	assert.Equal(t, categories.StartApproval, reqs[0].Category, "card category")
	assert.Equal(t, pr.RequestRef, reqs[0].RequestRef, "card requestRef")
	assert.Equal(t, channelevents.AudienceApprovers, reqs[0].Audience.Scope, "audience scope")
	require.Len(t, reqs[0].Audience.Approvers, 1, "approver fan-out")
	assert.Equal(t, "user:YWRtaW5AZXhhbXBsZS5jb20", reqs[0].Audience.Approvers[0].Subject.String(),
		"approver is the platform admin, addressed by canonical subject")
}

// TestStartGate_MemberUnaffected: a stamped org member keeps today's exact
// behavior — routed, started_by written, nothing parked, no start_session check.
func TestStartGate_MemberUnaffected(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)
	calls := startSessionRefused(az) // would refuse IF asked; a member must not be asked

	id := guestIdentity("U_MEMBER")
	id.OrgMembership = channelkinds.OrgMembershipMember
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel: ch, ExternalIDs: id, ChannelKey: "thread:C1:1", MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	assert.Equal(t, 1, az.startedByCalls, "started_by written as before")
	assert.Empty(t, *calls, "no start_session check for an org member")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	assert.False(t, spiceboxv1alpha1.StartApprovalPending(&sessions.Items[0]), "no parking annotation")
	assert.Empty(t, sessions.Items[0].Status.PendingRequesters, "no pending entry")
}

// TestStartGate_PermittedGuestPasses: a guest holding agentclass#start_session
// (a standing `starter` grant) starts without approval — the documented
// override path.
func TestStartGate_PermittedGuestPasses(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)
	az.checkOwnerFn = func(_, _, _ string) (bool, error) { return true, nil }

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel: ch, ExternalIDs: guestIdentity("U_GUEST"), ChannelKey: "thread:C1:1", MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	assert.Equal(t, 1, az.startedByCalls, "started_by written")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	assert.False(t, spiceboxv1alpha1.StartApprovalPending(&sessions.Items[0]), "no parking annotation")
}

// TestStartGate_EmptyStampOnAttributingChannelParks: fail-closed — a channel
// whose kind attributes membership delivering an UNSTAMPED identity reads as
// guest. A dropped stamp must gate, never widen.
func TestStartGate_EmptyStampOnAttributingChannelParks(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)
	startSessionRefused(az)
	az.lookupSubjectsResult = []string{"YWRtaW5AZXhhbXBsZS5jb20"}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U_NOSTAMP", Email: "n@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "unstamped identity must park")
	assert.Equal(t, 0, az.startedByCalls, "no started_by tuple")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	assert.True(t, spiceboxv1alpha1.StartApprovalPending(&sessions.Items[0]), "parked")
}

// TestStartGate_NonAttributingChannelKeepsPreGateBehavior: an ordinary fake
// channel (no orgScoped) with an unstamped identity is untouched by the gate.
func TestStartGate_NonAttributingChannelKeepsPreGateBehavior(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)
	calls := startSessionRefused(az)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	assert.Empty(t, *calls, "gate must not run on a non-attributing channel")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	assert.False(t, spiceboxv1alpha1.StartApprovalPending(&sessions.Items[0]))
}

// TestStartGate_ExplicitGuestStampHonoredEvenWithoutAttribution: an explicit
// guest stamp gates even on a channel whose kind does not declare attribution.
// Nothing legitimate stamps guest and then expects a free pass.
func TestStartGate_ExplicitGuestStampHonoredEvenWithoutAttribution(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch)
	startSessionRefused(az)
	az.lookupSubjectsResult = []string{"YWRtaW5AZXhhbXBsZS5jb20"}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel: ch, ExternalIDs: guestIdentity("U_GUEST"), ChannelKey: "thread:C1:1", MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "explicit guest stamp must gate")
	assert.Equal(t, 0, az.startedByCalls, "no started_by tuple")
}

// TestStartGate_CheckErrorFailsClosed: an errored start_session check refuses
// the inbound outright — no session, no tuple, no card. A SpiceDB blip must
// never mint a session for a guest.
func TestStartGate_CheckErrorFailsClosed(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)
	az.checkOwnerFn = func(_, _, _ string) (bool, error) {
		return false, assert.AnError
	}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel: ch, ExternalIDs: guestIdentity("U_GUEST"), ChannelKey: "thread:C1:1", MessageText: "hello",
	})
	require.Error(t, err, "check error must propagate")
	assert.Equal(t, channelkinds.OutcomeInternalError, dec.Outcome, "outcome")
	assert.Equal(t, 0, az.startedByCalls, "no started_by tuple")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Empty(t, sessions.Items, "no session on an errored gate check")
}

// TestStartGate_NoAddressableAdminRefuses: a guest without start_session on a
// cluster with no addressable platform admin is refused cleanly — no orphan
// parked session nobody can ever approve — and the monitoring channel is told.
func TestStartGate_NoAddressableAdminRefuses(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	p, az, _, _, cli := newPipeline(t, ch)
	startSessionRefused(az)
	// az.lookupSubjectsResult deliberately empty: no admins resolvable.

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel: ch, ExternalIDs: guestIdentity("U_GUEST"), ChannelKey: "thread:C1:1", MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "refused")
	require.NotNil(t, dec.Notice, "the guest must be told, not ghosted")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Empty(t, sessions.Items, "no unapprovable parked session")
}

// parkedStartSession builds an AgentSession parked awaiting start approval for
// the given guest identity, correlated to channel c1 / key "thread:C1:1".
// withEntry controls whether the durable PendingRequesters entry exists — the
// false case models a channelsd crash between the Create and the status patch.
func parkedStartSession(t *testing.T, name string, guest channelkinds.ExternalIdentity, withEntry bool) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	const requestRef = "startappr-park-1"
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  sha256HexTest("thread:C1:1"),
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartApprovalRequestRef: requestRef,
				spiceboxv1alpha1.AnnotationStartedByExternalID:     guest.ExternalID.String(),
				spiceboxv1alpha1.AnnotationStartedByCanonicalID:    canonicalID(guest).Subject().String(),
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: "fake", Key: "thread:C1:1",
				NATSSubjectPrefix: "ap.session.default." + name,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval},
	}
	if withEntry {
		sess.Status.PendingRequesters = []spiceboxv1alpha1.PendingRequester{{
			Kind: guest.Kind.String(), ExternalID: guest.ExternalID.String(), Email: guest.Email.String(),
			RequestRef: requestRef, Category: categories.StartApproval,
			RequestedAt: metav1.Now(), ChannelKey: "thread:C1:1",
		}}
	}
	return sess
}

// TestStartGate_ParkedStarterFollowupSuppressed: the guest messaging their own
// parked thread again is told nothing new (suppressed; the card's public note
// already said "awaiting approval") — no duplicate pending entry, no second
// admin card, and still no standing written.
func TestStartGate_ParkedStarterFollowupSuppressed(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	guest := guestIdentity("U_GUEST")
	sess := parkedStartSession(t, "s-parked", guest, true)
	p, az, _, nats, cli := newPipeline(t, ch, sess)
	az.lookupSubjectsResult = []string{"YWRtaW5AZXhhbXBsZS5jb20"}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel: ch, ExternalIDs: guest, ChannelKey: "thread:C1:1", MessageText: "any update?",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "outcome")
	assert.Equal(t, 0, az.startedByCalls, "still no started_by tuple")
	assert.Empty(t, publishedInteractionRequests(t, nats), "no second admin card")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "s-parked"}, &got))
	assert.Len(t, got.Status.PendingRequesters, 1, "no duplicate pending entry")
}

// TestStartGate_ParkedStarterFollowupReRaisesLostRequest: the crash window —
// annotation stamped on the Create but the status patch (or publish) was lost.
// The starter's next message re-raises: the durable entry is stamped under the
// SAME requestRef the annotation carries, and the admin card is published.
func TestStartGate_ParkedStarterFollowupReRaisesLostRequest(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	guest := guestIdentity("U_GUEST")
	sess := parkedStartSession(t, "s-parked", guest, false)
	p, az, _, nats, cli := newPipeline(t, ch, sess)
	az.lookupSubjectsResult = []string{"YWRtaW5AZXhhbXBsZS5jb20"}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel: ch, ExternalIDs: guest, ChannelKey: "thread:C1:1", MessageText: "hello again",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "outcome")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "s-parked"}, &got))
	require.Len(t, got.Status.PendingRequesters, 1, "lost pending entry re-stamped")
	assert.Equal(t, "startappr-park-1", got.Status.PendingRequesters[0].RequestRef,
		"re-raise must reuse the annotation's requestRef so a click resolves it")

	reqs := publishedInteractionRequests(t, nats)
	require.Len(t, reqs, 1, "admin card re-published")
	assert.Equal(t, categories.StartApproval, reqs[0].Category)
	assert.Equal(t, "startappr-park-1", reqs[0].RequestRef)
}

// TestStartGate_ParkedStrangerRefused: someone ELSE messaging the parked
// thread is refused with a notice — never routed into the interact/join flow,
// whose approver seat (started_by) holds no tuple here and would misfire.
func TestStartGate_ParkedStrangerRefused(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	guest := guestIdentity("U_GUEST")
	sess := parkedStartSession(t, "s-parked", guest, true)
	p, az, _, nats, cli := newPipeline(t, ch, sess)

	stranger := channelkinds.ExternalIdentity{
		Kind: "fake", ExternalID: "U_OTHER", Email: "other@example.com",
		OrgMembership: channelkinds.OrgMembershipMember,
	}
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel: ch, ExternalIDs: stranger, ChannelKey: "thread:C1:1", MessageText: "what is this?",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "outcome")
	require.NotNil(t, dec.Notice, "the stranger must be told the session is awaiting approval")
	assert.Empty(t, publishedInteractionRequests(t, nats), "no join card for a parked session")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "s-parked"}, &got))
	assert.Len(t, got.Status.PendingRequesters, 1, "no join entry added")
	assert.Equal(t, 0, az.deniedTouchCalls, "no denied tuple for a bystander")
}

// TestStartGate_ServiceSubjectSkipsGate: a no-user-identity inbound acting as
// a declared service subject (cron/bento shape) is governed by the existing
// non-human paths, not the org gate — even on an attributing channel.
func TestStartGate_ServiceSubjectSkipsGate(t *testing.T) {
	ch := newOrgScopedChannel("c1")
	ch.Spec.Role = spiceboxv1alpha1.ChannelRoleBoth
	ch.Spec.AuthzSubject = "service:cron-reporter"
	p, az, _, _, cli := newPipeline(t, ch)
	calls := startSessionRefused(az)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ExternalIDs:  channelkinds.ExternalIdentity{}, // no per-user identity
		AuthzSubject: "service:cron-reporter",         // the listener-stamped acting subject
		ChannelKey:   "thread:C1:1",
		MessageText:  "tick",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	assert.Empty(t, *calls, "org gate must not run for a service subject")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	assert.False(t, spiceboxv1alpha1.StartApprovalPending(&sessions.Items[0]))
}
