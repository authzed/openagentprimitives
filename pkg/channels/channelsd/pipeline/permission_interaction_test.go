// pkg/channels/channelsd/pipeline/permission_interaction_test.go
//
// Two halves of the session-join flow: handlePermissionDeny, which publishes
// the interaction_request(permission_request) and stamps the durable
// Status.PendingRequesters record an Approve/Deny click resolves against; and
// decidePermission, the bound handler for that click.
//
// decidePermission tests call p.decidePermission directly rather than going
// through HandleInteractionDecision: the handler performs no standing check of
// its own, because the generic pipe already ran the category's DeciderPolicy
// check before invoking it (see decidePermission's doc comment).
package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// TestHandlePermissionDenyPublishesInteractionRequest: a CheckInteract-false
// active session must publish an interaction_request(permission_request)
// envelope (delivered through the outbound relay's "interaction" sub-channel,
// same as portal_access) AND durably record the pending join in
// Status.PendingRequesters, with RequestRef matching the envelope's — that
// match is what lets a later decision resolve the entry it applies to.
func TestHandlePermissionDenyPublishesInteractionRequest(t *testing.T) {
	sess := existingSession(t, "c1-perm", "UOWNER", spiceboxv1alpha1.AgentSessionPhaseRunning)
	// Canonical id deliberately differs from the raw AnnotationStartedByExternalID
	// ("UOWNER") set above — proves the pipeline doesn't accidentally read the
	// canonical annotation for Approvers[0].ExternalID: the wire ExternalID
	// field is honestly raw (identity.RawExternalID), sourced from
	// AnnotationStartedByExternalID directly; the Slack interaction sender
	// derives the canonical itself (via Principal().AllowSynthetic().Canonical())
	// before calling resolveSlackUserIDFromCanonical — see
	// pkg/channels/channelkinds/slack's TestInteractionSender_Request_RequesterRawIDWithEmail_ResolvesViaDerivedCanonical
	// for that consumer-side proof.
	sess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = "owner@example.com"
	// Distinct from both the raw ("UOWNER") and canonical ("owner@example.com")
	// annotations above — proves Approvers[0].Email below is sourced from
	// AnnotationStartedByEmail specifically, not aliased to one of the other
	// two started-by annotations.
	sess.Annotations[spiceboxv1alpha1.AnnotationStartedByEmail] = "verified-owner@example.com"
	p, az, _, nats, _ := newPipeline(t, sess)
	az.checkResult = false // CheckInteract=false → the deny path this test exercises

	ev := channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "UREQ", Email: "requester@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "let me in",
	}

	dec, err := p.handlePermissionDeny(context.Background(), sess, ev)
	require.NoError(t, err, "handlePermissionDeny")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "dec.Outcome")
	assert.True(t, dec.Notice.IsSuppressed(), "notice suppressed: the interaction sender owns the in-thread note")

	env := findEnvelopeBySubjectSuffix(t, nats, ".out."+string(channelevents.KindInteractionRequest))
	var req channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &req), "unmarshal InteractionRequestPayload")

	assert.Equal(t, categories.PermissionRequest, req.Category, "Category")
	assert.Equal(t, sess.Namespace, req.AgentSessionRef.Namespace, "AgentSessionRef.Namespace")
	assert.Equal(t, sess.Name, req.AgentSessionRef.Name, "AgentSessionRef.Name")
	assert.NotEmpty(t, req.RequestRef, "RequestRef is set")
	assert.NotEmpty(t, req.Lead, "Lead is set")
	assert.Equal(t, channelevents.AudienceApprovers, req.Audience.Scope, "Audience.Scope")
	require.Len(t, req.Audience.Approvers, 1, "exactly one approver (the owner)")
	assert.Equal(t, "slack", req.Audience.Approvers[0].Kind.String(), "approver Kind mirrors the requester's channel kind")
	// Approvers[0].ExternalID must be the RAW started-by external id
	// (AnnotationStartedByExternalID, "UOWNER") — honestly raw, never the
	// precomputed canonical. Stuffing a canonical into this field is the
	// exact anti-pattern typed-identity slice 3 eliminates (ExternalID is
	// now identity.RawExternalID, which can't hold a canonical without an
	// explicit, suspicious conversion). Delivery correctness now lives in
	// the consumer's derivation (Principal().AllowSynthetic().Canonical()),
	// proven separately in pkg/channels/channelkinds/slack.
	assert.Equal(t, "UOWNER", req.Audience.Approvers[0].ExternalID.String(), "approver ExternalID = raw started-by external id")
	assert.Empty(t, req.Audience.Approvers[0].Subject, "Subject unset: email is present, so no synthetic-canonical fallback is needed")
	// Regression pin: Approvers[0].Email must be populated from
	// AnnotationStartedByEmail (the verified owner email) so identity
	// consumers that key off Email (e.g. the e2e harness's identityHandle,
	// which prefers Email over ExternalID when deriving a join prompt's
	// StartedBy) resolve to a real, matchable address instead of falling
	// back to the base64 canonical blob in ExternalID.
	assert.Equal(t, "verified-owner@example.com", req.Audience.Approvers[0].Email.String(), "approver Email = verified owner email from AnnotationStartedByEmail")
	assert.True(t, req.Audience.PublicNote, "PublicNote")
	assert.Contains(t, req.Audience.PublicNoteBody, "awaiting approval", "PublicNoteBody carries the neutral 'awaiting approval' framing")
	// The note names neither party in its TEXT. Asserting "<@UREQ>" / "<@UOWNER>"
	// here would read as coverage and be none: the string is the pre-render wire
	// value, and every surface renders PublicNoteBody inert, so such mentions
	// reach the channel as the literal "&lt;@UREQ&gt;". Who to wait on is
	// declared structurally in Audience.Approvers below and rendered live by the
	// surface; see TestHandlePermissionDeny_NamesIdentitiesStructurallyNotAsMarkup.
	assert.NotContains(t, req.Audience.PublicNoteBody, "<@", "PublicNoteBody is inert text, not mention markup")
	require.Len(t, req.Actions, 2, "Approve + Deny")
	assert.Equal(t, "approve", req.Actions[0].ID)
	assert.Equal(t, "deny", req.Actions[1].ID)
	require.NoError(t, req.Validate(), "published payload must be wire-valid")

	// Durable pending-join record, keyed so the bound decision handler can find
	// it: RequestRef matches the published envelope's RequestRef.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, p.K8s.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "Get after handlePermissionDeny")
	require.Len(t, got.Status.PendingRequesters, 1, "one durable pending-join record")
	pr := got.Status.PendingRequesters[0]
	assert.Equal(t, req.RequestRef, pr.RequestRef, "PendingRequester.RequestRef must match the published interaction_request's RequestRef")
	assert.Equal(t, "slack", pr.Kind, "PendingRequester.Kind")
	assert.Equal(t, "UREQ", pr.ExternalID, "PendingRequester.ExternalID")
	assert.Equal(t, "requester@example.com", pr.Email, "PendingRequester.Email")
	assert.Equal(t, "let me in", pr.MessageText, "PendingRequester.MessageText stashed for resubmit-on-approve")
	assert.Equal(t, 1, az.deniedCalls, "CheckDenied consulted once (the blocklist gate ahead of the dedup/publish path)")
}

// requesterMentionField returns the Field that declares WHO is asking to
// join, or nil when the payload names them nowhere structural.
func requesterMentionField(req channelevents.InteractionRequestPayload) *channelevents.InteractionField {
	for i, f := range req.Fields {
		if len(f.Mentions) > 0 {
			return &req.Fields[i]
		}
	}
	return nil
}

// TestHandlePermissionDeny_NamesIdentitiesStructurallyNotAsMarkup
//
// A mention is a structured IDENTITY; a body is untrusted TEXT. This pipeline
// is channel-agnostic and cannot know which of its strings a surface parses as
// markup, so it must never hand a surface pre-rendered channel-native mention
// syntax. Both fields below are documented on the wire type as rendered inert,
// and the Slack kind escapes both (escapePublisherPayload for Body,
// publicNoteText for PublicNoteBody), so an interpolated "<@U123>" reaches the
// reader as the literal "&lt;@U123&gt;" — the join note names nobody clickably
// and the approver DM's copy is visibly mangled.
//
// Identities are declared where a surface can resolve them instead:
// Audience.Approvers (the Slack kind renders its own live "Waiting on …"
// clause) for the approver, and InteractionField.Mentions — which carries the
// FULL structured identity and is deliberately exempt from escaping, because
// that value is markup the KIND composed — for the requester.
//
// pkg/channels/channelkinds/slack's TestPublicNote_ApproverMentionsStayLive and
// TestInteractionSender_Request_ResolvesFieldMentions prove the other half:
// that a structurally-declared identity does come out live.
func TestHandlePermissionDeny_NamesIdentitiesStructurallyNotAsMarkup(t *testing.T) {
	sess := existingSession(t, "c1-perm-struct", "UOWNER", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, az, _, nats, _ := newPipeline(t, sess)
	az.checkResult = false

	ev := channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "UREQ", Email: "requester@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "let me in",
	}
	dec, err := p.handlePermissionDeny(context.Background(), sess, ev)
	require.NoError(t, err, "handlePermissionDeny")
	require.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "dec.Outcome")

	env := findEnvelopeBySubjectSuffix(t, nats, ".out."+string(channelevents.KindInteractionRequest))
	var req channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &req), "unmarshal InteractionRequestPayload")

	// The inert fields carry no channel-native mention markup.
	assert.NotContains(t, req.Audience.PublicNoteBody, "<@",
		"PublicNoteBody is rendered inert — an interpolated mention lands as &lt;@…&gt;")
	assert.NotContains(t, req.Body, "<@",
		"Body is escaped by escapePublisherPayload — an interpolated mention lands as &lt;@…&gt;")

	// ...because both identities are declared structurally instead.
	require.Len(t, req.Audience.Approvers, 1, "the approver is declared in Audience.Approvers")
	assert.Equal(t, "UOWNER", req.Audience.Approvers[0].ExternalID.String(),
		"the Slack kind renders this list as its own live 'Waiting on …' clause")

	f := requesterMentionField(req)
	require.NotNil(t, f, "the requester must be declared via InteractionField.Mentions, not interpolated into prose")
	require.Len(t, f.Mentions, 1, "exactly the one requester")
	assert.Equal(t, "UREQ", f.Mentions[0].ExternalID.String(), "raw external id, per ExternalIdentity's contract")
	assert.Equal(t, "requester@example.com", f.Mentions[0].Email.String(),
		"email rides along so a surface can resolve a canonical the raw id alone cannot")
	assert.NotEmpty(t, f.Value,
		"Value is the always-present display fallback for surfaces that do not resolve mentions")
	require.NoError(t, req.Validate(), "published payload must be wire-valid")
}

// TestHandlePermissionDeny_RequesterMessageTravelsAsExcerpt: the requester's
// own message is UNTRUSTED text and must travel in the ONE slot the wire
// contract reserves for that — InteractionExcerpt, which every surface renders
// inert and visually set off (channelevents.InteractionExcerpt's CONTRACT) —
// never interpolated into Body, which the same contract calls
// publisher-authored and trusted.
//
// It regressed as a rendering bug before it was read as a contract one. The
// preview was italicized inline ("Their message: _%s_"), and a join request's
// raw text routinely spans a newline — the bot mention on line 1, the ask on
// line 2. Slack mrkdwn italics do not cross a newline, so the approver read the
// delimiters themselves:
//
//	Their message: _<@U0BL99R25K3>
//	can you share the performance numbers?_
//
// The fix cannot be a "> " blockquote written HERE. This pipeline is
// channel-agnostic and the Slack kind escapes the whole Body at the wire
// boundary (escapePublisherPayload → inertProse → escapeSlackText, which maps
// ">" to "&gt;"), so a marker composed upstream of that sweep arrives dead —
// the same order property renderMonitoringText turns on when it applies its
// blockquote prefix AFTER escaping. A surface that wants a quote composes it
// from the structured Excerpt itself.
func TestHandlePermissionDeny_RequesterMessageTravelsAsExcerpt(t *testing.T) {
	sess := existingSession(t, "c1-perm-excerpt", "UOWNER", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, az, _, nats, _ := newPipeline(t, sess)
	az.checkResult = false // CheckInteract=false → the deny path this test exercises

	// Multi-line, exactly as a Slack join request arrives: the bot mention the
	// requester typed, then the ask. This is the shape the italics broke on.
	const msg = "<@U0BL99R25K3>\ncan you share the performance numbers for each of the permissions system?"
	ev := channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "UREQ", Email: "requester@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: msg,
	}

	dec, err := p.handlePermissionDeny(context.Background(), sess, ev)
	require.NoError(t, err, "handlePermissionDeny")
	require.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "dec.Outcome")

	env := findEnvelopeBySubjectSuffix(t, nats, ".out."+string(channelevents.KindInteractionRequest))
	var req channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &req), "unmarshal InteractionRequestPayload")

	require.NotNil(t, req.Excerpt, "the requester's message must travel as an Excerpt")
	assert.Equal(t, "Their message", req.Excerpt.Label, "Excerpt.Label names the quoted region")
	assert.Equal(t, msg, req.Excerpt.Content,
		"Excerpt.Content is the requester's message verbatim — the surface makes it inert, the pipeline does not pre-mangle it")

	// Body keeps the publisher's own prose and nothing of the requester's.
	assert.NotContains(t, req.Body, "performance numbers",
		"the requester's text must NOT be interpolated into Body, which the wire contract calls trusted publisher prose")
	assert.NotContains(t, req.Body, "Their message",
		"the label belongs to the Excerpt, so Body must not introduce a preview of its own")
	assert.NotContains(t, req.Body, "<@",
		"Body is escaped by escapePublisherPayload — an interpolated mention lands as &lt;@…&gt;")
	require.NoError(t, req.Validate(), "published payload must be wire-valid")
}

// TestHandlePermissionDeny_AttachmentsNotCarried_MentionedInPublicNote:
// PendingRequester (seen above) stashes only MessageText, and
// permission_interaction.go's approve-replay rebuilds a synthetic InboundEvent
// with no Attachments field at all, so a file attached to a denied join request
// would otherwise vanish with no mention anywhere. The requester-facing Notice
// is deliberately suppressed (PublicNoteBody is the interaction sender's job),
// so the attachment mention has to land in PublicNoteBody.
func TestHandlePermissionDeny_AttachmentsNotCarried_MentionedInPublicNote(t *testing.T) {
	sess := existingSession(t, "c1-perm-attach", "UOWNER", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, az, _, nats, _ := newPipeline(t, sess)
	az.checkResult = false // CheckInteract=false → the deny path this test exercises

	ev := channelkinds.InboundEvent{
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "UREQ", Email: "requester@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "let me in",
		Attachments: oneAttachment("F1", "id-badge.png", "image/png", 1024),
	}

	dec, err := p.handlePermissionDeny(context.Background(), sess, ev)
	require.NoError(t, err, "handlePermissionDeny")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)

	env := findEnvelopeBySubjectSuffix(t, nats, ".out."+string(channelevents.KindInteractionRequest))
	var req channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &req), "unmarshal InteractionRequestPayload")
	assert.Contains(t, req.Audience.PublicNoteBody, "won't carry through once approved",
		"the public note must tell the requester their attachment did not come along")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, p.K8s.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "Get after handlePermissionDeny")
	require.Len(t, got.Status.PendingRequesters, 1)
	assert.Equal(t, "let me in", got.Status.PendingRequesters[0].MessageText,
		"the stashed record still carries only text — full attachment carry-through is a follow-up, not this fix")
}

// ---------------------------------------------------------------------------
// decidePermission — the bound decision handler
// ---------------------------------------------------------------------------

// newPermissionPipeline builds a *Pipeline over a fake k8s client seeded
// with a Running AgentSession owned by U_OWNER — the fixture decidePermission's
// tests exercise. Returns the fakeAuthz too so tests can assert the interact
// grant (or its absence).
func newPermissionPipeline(t *testing.T) (*Pipeline, *spiceboxv1alpha1.AgentSession, *fakeAuthz) {
	t.Helper()
	sess := existingSession(t, "c1-perm-decide", "U_OWNER", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, az, _, _, _ := newPipeline(t, sess)
	return p, sess, az
}

// seedPendingRequester appends one PendingRequesters entry (keyed by
// requestRef) to sess's persisted status through the same WriteOwned path
// production uses (seedApprovalStatus), mirroring the record
// handlePermissionDeny stamps. messageText/channelKey mirror the stash
// handlePermissionDeny writes for resubmit-on-approve; most callers pass "" for
// both since they don't exercise that branch.
func seedPendingRequester(t *testing.T, p *Pipeline, sess *spiceboxv1alpha1.AgentSession, requestRef string, requester channelkinds.ExternalIdentity, messageText, channelKey string) {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, p.K8s.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "get session before seeding pending requester")
	got.Status.PendingRequesters = append(got.Status.PendingRequesters, spiceboxv1alpha1.PendingRequester{
		Kind:        requester.Kind.String(),
		TeamScope:   requester.TeamScope.String(),
		ExternalID:  requester.ExternalID.String(),
		Email:       requester.Email.String(),
		RequestRef:  requestRef,
		RequestedAt: metav1.Now(),
		MessageText: messageText,
		ChannelKey:  channelKey,
	})
	seedApprovalStatus(t, p.K8s, &got)
}

// TestDecidePermissionApproveGrantsAndClears verifies the approve leg:
// the requester is granted `interact` (via grantInteract → Engine →
// TouchInteractParticipantUser), the matching PendingRequesters entry is
// cleared, and the PermissionRequestPending condition flips False once no
// requesters remain.
func TestDecidePermissionApproveGrantsAndClears(t *testing.T) {
	p, sess, az := newPermissionPipeline(t)
	requester := channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "UREQ", Email: "requester@example.com"}
	seedPendingRequester(t, p, sess, "perm-1", requester, "", "")
	wantCanonical := requesterCanonical(spiceboxv1alpha1.PendingRequester{
		Kind: requester.Kind.String(), ExternalID: requester.ExternalID.String(), Email: requester.Email.String(),
	})

	out, err := p.decidePermission(context.Background(), channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   categories.PermissionRequest,
			RequestRef: "perm-1",
			ActionID:   "approve",
			Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER", Email: "owner@example.com"},
		},
	})
	require.NoError(t, err, "decidePermission")
	assert.Equal(t, channelevents.OutcomeApproved, out.Result, "Outcome.Result")
	assert.Equal(t, "Approved", out.OutcomeText, "Outcome.OutcomeText")
	require.NoError(t, out.Validate(), "decidePermission must return a valid Outcome")
	assert.True(t, az.hasParticipantUser(wantCanonical), "requester granted interact")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, p.K8s.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "Get after decidePermission")
	assert.Empty(t, got.Status.PendingRequesters, "pending entry cleared on decision")
	cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending)
	if assert.NotNil(t, cond, "PermissionRequestPending condition") {
		assert.Equal(t, metav1.ConditionFalse, cond.Status, "condition flips False once no requesters remain")
	}
}

// TestDecidePermissionDenyClearsWithoutGranting verifies the deny leg:
// the pending entry is cleared, but grantInteract is never called.
func TestDecidePermissionDenyClearsWithoutGranting(t *testing.T) {
	p, sess, az := newPermissionPipeline(t)
	requester := channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "UREQ", Email: "requester@example.com"}
	seedPendingRequester(t, p, sess, "perm-2", requester, "", "")

	out, err := p.decidePermission(context.Background(), channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   categories.PermissionRequest,
			RequestRef: "perm-2",
			ActionID:   "deny",
			Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER", Email: "owner@example.com"},
		},
	})
	require.NoError(t, err, "decidePermission")
	assert.Equal(t, channelevents.OutcomeDenied, out.Result, "Outcome.Result")
	assert.Equal(t, "Denied", out.OutcomeText, "Outcome.OutcomeText")
	assert.Empty(t, az.participantUsers, "deny must not grant interact")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, p.K8s.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "Get after decidePermission")
	assert.Empty(t, got.Status.PendingRequesters, "pending entry cleared on decision")
}

// TestDecidePermissionDenyBlocksRequester: the deny leg must actually block the
// requester by writing the SpiceDB denied relation via denyInteract, so
// CheckDenied's blocklist gate (handlePermissionDeny) finds a populated gate on
// the requester's next message. Without the write, every later message re-mints
// an interaction_request and re-DMs the owner, forever.
func TestDecidePermissionDenyBlocksRequester(t *testing.T) {
	p, sess, az := newPermissionPipeline(t)
	requester := channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "UREQ", Email: "requester@example.com"}
	seedPendingRequester(t, p, sess, "perm-3", requester, "", "")
	wantCanonical := requesterCanonical(spiceboxv1alpha1.PendingRequester{
		Kind: requester.Kind.String(), ExternalID: requester.ExternalID.String(), Email: requester.Email.String(),
	})

	out, err := p.decidePermission(context.Background(), channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   categories.PermissionRequest,
			RequestRef: "perm-3",
			ActionID:   "deny",
			Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER", Email: "owner@example.com"},
		},
	})
	require.NoError(t, err, "decidePermission")
	assert.Equal(t, channelevents.OutcomeDenied, out.Result, "Outcome.Result")
	assert.Empty(t, az.participantUsers, "deny must not grant interact")
	assert.Equal(t, 1, az.deniedTouchCalls, "deny must write the SpiceDB denied relation exactly once")
	assert.Equal(t, wantCanonical.String(), az.lastDeniedUser, "denied relation written for the requester's canonical subject — the same identity CheckDenied reads back")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, p.K8s.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "Get after decidePermission")
	assert.Empty(t, got.Status.PendingRequesters, "pending entry cleared on decision")
}

// TestDecidePermissionDuplicateIsSpectator verifies idempotency: a
// requestRef with no matching PendingRequesters entry (already resolved,
// or never existed) renders the resolved outcome without a re-grant.
func TestDecidePermissionDuplicateIsSpectator(t *testing.T) {
	p, sess, az := newPermissionPipeline(t) // no seeded pending entry

	out, err := p.decidePermission(context.Background(), channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   categories.PermissionRequest,
			RequestRef: "gone",
			ActionID:   "approve",
			Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER"},
		},
	})
	require.NoError(t, err, "decidePermission")
	assert.Equal(t, channelevents.OutcomeApproved, out.Result, "renders the resolved outcome; no re-grant")
	assert.Empty(t, az.participantUsers, "no grant on a stale/duplicate click")
}

// TestDecidePermissionApproveResubmitsStashedMessage pins the
// resubmit-on-approve branch: approving a join request whose PendingRequester
// carries a non-empty stashed MessageText must replay that message through the
// inbound pipeline — observed as a memory turn appended with the stashed text
// plus the wakeup envelope ResubmitAuthorized publishes on success — so the
// requester doesn't have to retype. A second requestRef with an empty
// MessageText (handlePermissionDeny's maxStashedText guard) pins the other
// side: no resubmit, so neither a memory turn nor a wakeup is added.
func TestDecidePermissionApproveResubmitsStashedMessage(t *testing.T) {
	sess := existingSession(t, "c1-perm-resubmit", "U_OWNER", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, az, mem, nats, _ := newPipeline(t, sess)
	requester := channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "UREQ", Email: "requester@example.com"}
	wantCanonical := requesterCanonical(spiceboxv1alpha1.PendingRequester{
		Kind: requester.Kind.String(), ExternalID: requester.ExternalID.String(), Email: requester.Email.String(),
	})

	// --- non-empty MessageText: resubmit fires ---
	seedPendingRequester(t, p, sess, "perm-resubmit-1", requester, "let me in", "thread:C1:1")

	out, err := p.decidePermission(context.Background(), channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   categories.PermissionRequest,
			RequestRef: "perm-resubmit-1",
			ActionID:   "approve",
			Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER", Email: "owner@example.com"},
		},
	})
	require.NoError(t, err, "decidePermission")
	assert.Equal(t, channelevents.OutcomeApproved, out.Result, "Outcome.Result")
	assert.True(t, az.hasParticipantUser(wantCanonical), "requester granted interact")

	require.Len(t, mem.appends, 1, "resubmit replays exactly one memory turn")
	appended := mem.appends[0]
	assert.Equal(t, sess.Namespace, appended.ns, "resubmit memory append: ns")
	assert.Equal(t, sess.Name, appended.name, "resubmit memory append: name")
	assert.Equal(t, "user", appended.turn.Role, "resubmitted turn attributed to the user")
	assert.Equal(t, "user:"+wantCanonical.String(), string(appended.turn.Author), "resubmitted turn's author is the requester's canonical subject")
	require.Len(t, appended.turn.Content, 1, "resubmitted turn has one content block")
	assert.Equal(t, "let me in", appended.turn.Content[0].Text, "resubmit replays the stashed MessageText verbatim")

	env := findEnvelopeBySubjectSuffix(t, nats, ".in."+string(channelevents.KindUserMessage))
	assert.Equal(t, channelevents.KindUserMessage, env.Kind, "resubmit publishes a wakeup user-message envelope")

	// --- empty MessageText: the guard skips resubmit entirely ---
	seedPendingRequester(t, p, sess, "perm-resubmit-2", requester, "", "")

	out2, err := p.decidePermission(context.Background(), channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   categories.PermissionRequest,
			RequestRef: "perm-resubmit-2",
			ActionID:   "approve",
			Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER", Email: "owner@example.com"},
		},
	})
	require.NoError(t, err, "decidePermission (empty MessageText)")
	assert.Equal(t, channelevents.OutcomeApproved, out2.Result, "Outcome.Result (empty MessageText)")
	assert.Len(t, mem.appends, 1, "empty MessageText must not append a second resubmit turn")
	assert.Len(t, nats.subjects, 1, "empty MessageText must not publish a second wakeup envelope")
}

// TestBindPermissionHandler_RegistersDecidePermission proves
// BindPermissionHandler wires p.decidePermission as the bound handler for
// categories.PermissionRequest — the "Bind at process start" deliverable
// internal/cmd/channelsd/main.go relies on. Re-registers the real category row
// (mirrors bindRealIdentityChoiceCategory) since the process-global registry
// may have been wiped by an earlier test's resetInteractions cleanup.
func TestBindPermissionHandler_RegistersDecidePermission(t *testing.T) {
	resetInteractions(t)
	channelinteractions.Register(channelinteractions.Category{
		Name:      categories.PermissionRequest,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideOwner,
		Resurface: channelinteractions.ResurfaceNone,
		Surface:   channelinteractions.SurfaceDMOnly,
	})
	p, _, _ := newPermissionPipeline(t)

	BindPermissionHandler(p)

	h, ok := channelinteractions.HandlerFor(categories.PermissionRequest)
	require.True(t, ok, "BindPermissionHandler must bind a handler for permission_request")
	require.NotNil(t, h, "bound handler must not be nil")
}
