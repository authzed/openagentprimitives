package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestDeliverServiceSubject_CarriedOnTheSessionButNeverAsStartedBy pins the
// two halves of what the no-user-identity branch does with the Channel's
// declared subject, which are easy to confuse because one of them looks like
// the other:
//
//   - started_by stays UNSET. The schema declares `relation started_by: user`,
//     so a service subject there would assert a human who does not exist (and
//     lands as `user:service:<id>`, an object id SpiceDB rejects outright).
//     This half already held; it is asserted here so the other half cannot be
//     "fixed" later by relaxing it.
//   - the subject is stamped on the session as its own annotation. Collecting,
//     validating and canonicalizing it and then dropping it left the runner
//     with no acting principal at all, so every governed tool call in a
//     webhook- or cron-driven session authorized as the empty subject — a
//     malformed SpiceDB request, not a denial.
func TestDeliverServiceSubject_CarriedOnTheSessionButNeverAsStartedBy(t *testing.T) {
	const subj = "service:demo-digest-bot"
	ch := newBentoChannel("cron-subject", subj)
	p, az, _, _, cli := newPipeline(t, ch, newSlackOutputChannel("cron-subject-out", "C-SUBJ"))

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ExternalIDs:  channelkinds.ExternalIdentity{}, // no user identity
		ChannelKey:   "cron:cron-subject:1.0",
		MessageText:  "nightly digest",
		AuthzSubject: subj,
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Equal(t, 0, az.startedByCalls,
		"a service subject must not be written to started_by (schema allows only `user`)")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	sess := &sessions.Items[0]

	assert.Equal(t, subj, spiceboxv1alpha1.AuthzServiceSubject(sess).String(),
		"the session carries the subject the runner authorizes tool calls as")
	assert.Empty(t, spiceboxv1alpha1.StartedByCanonical(sess),
		"the started-by canonical stays empty: this session has no human starter")
	assert.Empty(t, spiceboxv1alpha1.StartedBySubject(sess),
		"the started-by subject stays empty for the same reason")
}

// TestDeliverServiceReturningToOwnArchivedSession_ForksWithoutInteractCheck
// pins the channel-as-boundary rule for service-triggered sessions. A github/
// cron channel records its AuthzServiceSubject on the session but NEVER a
// started_by (schema: `user` only) and never a human interact grant. So when
// the SAME service re-delivers on the SAME channel to its now-terminal session
// — a new PR event, a webhook redelivery — the inherit-fork must proceed
// WITHOUT consulting interact: the signed channel is the authorization
// boundary, exactly as the human-thread takeover branch already reasons.
//
// Before the fix this went to CheckInteract with the service canonical, which
// (a) SpiceDB rejects outright as a malformed user:service:<id> subject, and
// (b) could never satisfy interact anyway (user/group only) — so every
// webhook redelivery to a terminal service session wedged fail-closed and the
// PR was never re-reviewed.
func TestDeliverServiceReturningToOwnArchivedSession_ForksWithoutInteractCheck(t *testing.T) {
	const subj = "service:demo-webhook-bot"
	ch := newBentoChannel("c1", subj)
	archived := archivedSession(t, "c1-old", "pr:owner/repo#12017", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "")
	archived.Annotations[spiceboxv1alpha1.AnnotationAuthzServiceSubject] = subj
	p, az, _, _, _ := newPipeline(t, ch, newSlackOutputChannel("c1-out", "C1"), archived)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ExternalIDs:  channelkinds.ExternalIdentity{}, // no user identity — a service inbound
		ChannelKey:   "pr:owner/repo#12017",
		MessageText:  "re-review this PR",
		AuthzSubject: subj,
	})
	require.NoError(t, err, "Deliver must not error (the pre-fix InvalidArgument regression)")
	assert.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome,
		"the owning service re-delivering must fork its own archived session")
	assert.Equal(t, 0, az.checkCalls,
		"channel-as-boundary: the owning service is authorized by the channel, no interact check")
}

// TestDeliverServiceInbound_DifferentServiceDeniedWithoutInteractCall keeps
// the channel-as-boundary bypass tightly scoped: it applies ONLY when the
// inbound's acting subject matches the archived session's own
// AuthzServiceSubject. A different service reaching the same archived session
// must still be DENIED — the bypass can never let one principal fork another's
// session — but it must be denied WITHOUT calling CheckInteract.
//
// This was TestDeliverServiceInbound_DifferentServiceStillChecksInteract,
// which asserted the opposite of the second half (checkCalls == 1). That
// matched channelsd's fake, which just records the call and returns its
// configured result — but the real SpiceDB `interact` relation is user|group
// only, and checkUser sends the canonical verbatim as a "user:<canonical>"
// object id. A service-typed canonical (already "service:<id>") lands as
// object id "service:<id>" — a colon the object_id pattern rejects outright.
// So this call ALWAYS errors in production: not a maybe, a guarantee, by
// construction of checkUser and the object_id grammar. That guaranteed error
// is exactly what orphaned check-run 104336839544 for six days:
// pipeline.Deliver returned an error, no session was created, and nothing
// was ever left to resolve the check run. The fix recognizes a non-user
// subject before the call, not after it fails.
func TestDeliverServiceInbound_DifferentServiceDeniedWithoutInteractCall(t *testing.T) {
	const owner = "service:demo-webhook-bot"
	const other = "service:some-other-bot"
	ch := newBentoChannel("c1", other)
	archived := archivedSession(t, "c1-old", "pr:owner/repo#12017", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "")
	archived.Annotations[spiceboxv1alpha1.AnnotationAuthzServiceSubject] = owner
	p, az, _, _, _ := newPipeline(t, ch, newSlackOutputChannel("c1-out", "C1"), archived)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ExternalIDs:  channelkinds.ExternalIdentity{},
		ChannelKey:   "pr:owner/repo#12017",
		MessageText:  "re-review",
		AuthzSubject: other,
	})
	require.NoError(t, err, "denied is a clean outcome, not an error")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome,
		"a non-owning service subject is still denied — the security outcome is unchanged")
	assert.Equal(t, 0, az.checkCalls,
		"a service subject can never hold interact (user|group only); must not reach SpiceDB at all")
}

// TestDeliverHumanInbound_ReachingArchivedServiceSession_StillChecksInteract
// pins the guard's precision: it is keyed on the SUBJECT TYPE (service,
// agentsession, ...), not on "not the owning service". A genuine human
// reaching a service-owned archived session is a bare user canonical — no
// SpiceDB object type prefix — and CheckInteract can resolve that (a class-
// wide `participant@group:...#member` grant, for instance), so it must still
// be consulted. Guards against a future widening of the type check swallowing
// this case too.
func TestDeliverHumanInbound_ReachingArchivedServiceSession_StillChecksInteract(t *testing.T) {
	const owner = "service:demo-webhook-bot"
	ch := newBentoChannel("c1", owner)
	archived := archivedSession(t, "c1-old", "pr:owner/repo#12017", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "")
	archived.Annotations[spiceboxv1alpha1.AnnotationAuthzServiceSubject] = owner
	p, az, _, _, _ := newPipeline(t, ch, newSlackOutputChannel("c1-out", "C1"), archived)
	az.checkResult = true // SpiceDB grants, e.g. a class-wide group membership

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_human", Email: "human@example.test"},
		ChannelKey:  "pr:owner/repo#12017",
		MessageText: "please re-review",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome,
		"SpiceDB granted interact, so the human's fork proceeds")
	assert.Equal(t, 1, az.checkCalls,
		"a bare human canonical is a real user|group subject; CheckInteract must still run")
}

// TestDeliverHumanInbound_StampsNoServiceSubject is the other direction: an
// inbound that DID carry a human must leave the service-subject annotation
// off entirely. Present on such a session it would sit behind the requester as
// a fallback nobody intended, ready to attribute work to a service identity
// the moment attribution went missing — which is precisely when the call
// should deny instead.
func TestDeliverHumanInbound_StampsNoServiceSubject(t *testing.T) {
	ch := newChannel("human-subject")
	p, _, _, _, cli := newPipeline(t, ch)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{ExternalID: "U-HUMAN", Email: "reviewer@example.test"},
		ChannelKey:  "thread:C1:1.0",
		MessageText: "please take a look",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	assert.Empty(t, spiceboxv1alpha1.AuthzServiceSubject(&sessions.Items[0]),
		"a human-attributed session must carry no service-subject fallback")
}
