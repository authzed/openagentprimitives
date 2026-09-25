package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// snapshotCategories saves the current channelinteractions registry contents
// and returns a func that resets and re-registers them — used by every
// registry-touching test in this slice so a test that needs a bespoke
// category set (e.g. only "permission_request") doesn't leak that truncated
// registry into tests that run after it in the same package.
func snapshotCategories(t *testing.T) func() {
	t.Helper()
	saved := channelinteractions.All()
	return func() {
		channelinteractions.Reset()
		for _, c := range saved {
			channelinteractions.Register(c)
		}
	}
}

// interactionEnvelope marshals any interaction payload into a
// channelevents.Envelope of the given kind.
func interactionEnvelope(t *testing.T, kind channelevents.Kind, pl any) channelevents.Envelope {
	t.Helper()
	b, err := json.Marshal(pl)
	require.NoError(t, err, "marshal interaction payload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        kind,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "sess-1"},
		PublishedAt: time.Now().UTC(),
		Payload:     b,
	}
}

// credentialLinkRequestPayload returns a wire-valid credential_link
// InteractionRequestPayload with a single link action addressed to the
// given requester (raw slack id + verified email — the shape a real Slack
// listener stamps, per ExternalIdentity's "ExternalID is always raw" contract).
// The sender derives the canonical via Principal().AllowSynthetic().Canonical()
// itself, and the email wins that derivation. Stuffing a pre-computed canonical
// into ExternalID is the raw-vs-canonical mismatch the typed identity API makes
// a compile error elsewhere in this package.
func credentialLinkRequestPayload(rawExternalID identity.RawExternalID, email, linkURL string) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		Category:   "credential_link",
		RequestRef: "req-1",
		Lead:       "Connect your accounts",
		Body:       "GitHub access is needed to open the PR.",
		Fields:     []channelevents.InteractionField{{Label: "Service", Value: "GitHub"}},
		Actions: []channelevents.InteractionAction{
			{ID: "connect_github", Label: "Connect GitHub", Kind: channelevents.ActionKindLink, URL: linkURL, Style: channelevents.ActionStylePrimary},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{Kind: "slack", ExternalID: rawExternalID, Email: identity.Email(email)},
		},
	}
}

// TestInteractionSender_Request_CredentialLink_EphemeralWithChannelContext
// verifies the happy path: a credential_link interaction_request with a
// link action produces a Block-Kit message (via PostEphemeral) whose
// actions block carries a URL button with the action's URL + label.
func TestInteractionSender_Request_CredentialLink_EphemeralWithChannelContext(t *testing.T) {
	const (
		email    = "alice@example.com"
		slackID  = "U_ALICE"
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
		linkURL  = "https://identityd.example.com/link?tok=abc123"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c}

	pl := credentialLinkRequestPayload(identity.RawExternalID(slackID), email, linkURL)
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, pl)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	require.Len(t, c.postEphemeralCalls, 1, "expected 1 PostEphemeral call")
	assert.Empty(t, c.postMessageCalls, "PostMessage must not be called when channel context is present")
	got := c.postEphemeralCalls[0]
	assert.Equal(t, chanID, got.channelID, "PostEphemeral channelID")
	assert.Equal(t, slackID, got.userID, "PostEphemeral userID")

	// thread_ts must be carried so the ephemeral lands in the session's thread.
	_, vals, err := slackapi.UnsafeApplyMsgOptions("test-token", chanID, "http://test.invalid/", got.options...)
	require.NoError(t, err, "UnsafeApplyMsgOptions")
	assert.Equal(t, threadTS, vals.Get("thread_ts"), "ephemeral must carry thread_ts")

	// Blocks aren't surfaced by UnsafeApplyMsgOptions (same limitation noted
	// in sender_credential_request_test.go); verify the rendering contract by
	// rebuilding via the same block builder the sender calls.
	blocks := buildInteractionRequestBlocks(pl, "default/sess-1")
	children := interactionChildren(t, blocks)
	require.GreaterOrEqual(t, len(children), 2, "section + actions inside the container")
	section, ok := children[0].(*slackapi.SectionBlock)
	require.True(t, ok, "block[0] must be *SectionBlock")
	// The lead lives in the container title; the body and fields in the section.
	assert.Contains(t, concatBlockText(blocks), "Connect your accounts")
	assert.Contains(t, section.Text.Text, "GitHub access is needed to open the PR.")
	assert.Contains(t, section.Text.Text, "Service")

	actions := interactionActionBlock(t, blocks)
	require.NotNil(t, actions, "the container must carry an action row")
	ok = true
	require.True(t, ok, "block[1] must be *ActionBlock")
	require.Len(t, actions.Elements.ElementSet, 1, "one button for the one link action")
	btn, ok := actions.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	require.True(t, ok, "element must be *ButtonBlockElement")
	assert.Equal(t, linkURL, btn.URL, "button URL must be the action's URL")
	assert.Equal(t, "Connect GitHub", btn.Text.Text, "button label must be the action's label")
	assert.Equal(t, slackapi.StylePrimary, btn.Style, "button style must map from ActionStylePrimary")
}

// TestInteractionSender_Request_RequesterRawIDWithEmail_ResolvesViaDerivedCanonical
// pins sendRequest's recipient resolution: a requester ExternalIdentity
// carrying a RAW, non-canonical channel-native ExternalID alongside a verified
// Email must resolve via the DERIVED canonical
// (r.Principal().AllowSynthetic().Canonical(), which the email always wins) —
// never by handing the raw ExternalID straight to
// resolveSlackUserIDFromCanonical. Passing the raw channel id would either fail
// to base64-decode or decode to unrelated garbage, silently dropping the
// delivery. The ExternalID here is deliberately NOT valid base64-RawURL, so a
// regression fails loudly (decode error) instead of coincidentally passing.
func TestInteractionSender_Request_RequesterRawIDWithEmail_ResolvesViaDerivedCanonical(t *testing.T) {
	const (
		rawID    = "Uraw!not-valid-base64" // "!" is outside the base64-RawURL alphabet
		email    = "x@example.com"
		slackID  = "U_DERIVED"
		chanID   = "C_IDCHOICE"
		threadTS = "1700000000.000002"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c}

	p := channelevents.InteractionRequestPayload{
		Category: "identity_choice", RequestRef: "idc-1", Lead: "Choose an identity",
		Actions: []channelevents.InteractionAction{
			{ID: "agent", Label: "Agent", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope: channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{
				Kind:       "slack",
				ExternalID: identity.RawExternalID(rawID),
				Email:      identity.Email(email),
			},
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send must succeed: the email must win the canonical derivation, not the raw ExternalID")

	require.Len(t, c.postEphemeralCalls, 1, "one ephemeral to the derived requester")
	assert.Equal(t, slackID, c.postEphemeralCalls[0].userID,
		"must resolve via the email-derived canonical (GetUserByEmailContext), never by decoding the raw ExternalID")
	require.Len(t, c.lookupByEmailCalls, 1, "exactly one email lookup")
	assert.Equal(t, email, c.lookupByEmailCalls[0], "the email lookup must use the verified Email, not anything derived from the raw ExternalID")
}

// TestInteractionSender_Request_RequesterSubjectOnly_Resolves pins that a
// Subject-only requester delivers: credential_request.go / portal_access.go
// build Audience.Requester Subject-only (Kind set, empty ExternalID, Subject
// carrying the precomputed canonical) whenever the started-by user has no
// verified email AND no raw ExternalID annotation on record. sendRequest's
// AudienceRequester gate must not reject `ExternalID == ""` before calling
// Principal(), which treats Subject as authoritative and resolves it fine. This
// drives the actual delivery path (not just InteractionRequestPayload.Validate,
// which already tolerated this shape) end to end through the fake Slack
// client.
func TestInteractionSender_Request_RequesterSubjectOnly_Resolves(t *testing.T) {
	const (
		email    = "carol@example.com"
		slackID  = "U_CAROL"
		chanID   = "C_SUBJECTONLY"
		threadTS = "1700000000.000003"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c}

	p := channelevents.InteractionRequestPayload{
		Category:   "credential_link",
		RequestRef: "req-subj-1",
		Lead:       "Connect your accounts",
		Actions: []channelevents.InteractionAction{
			{ID: "connect_accounts", Label: "Connect", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindLink, URL: "https://identityd.example.com/link"},
		},
		Audience: channelevents.InteractionAudience{
			Scope: channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{
				Kind:    "slack",
				Subject: identity.Subject("user:" + emailCanonical(email)),
			},
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send must succeed: a Subject-only requester must resolve via Principal(), not be rejected on empty ExternalID")

	require.Len(t, c.postEphemeralCalls, 1, "one ephemeral delivered to the Subject-derived requester")
	assert.Equal(t, slackID, c.postEphemeralCalls[0].userID,
		"must resolve via the Subject passthrough canonical (GetUserByEmailContext), not the empty ExternalID")
}

// TestInteractionSender_Request_RequesterIdPKind_ResolvesViaEmail pins that an
// AudienceRequester addressed with Kind "idp" (an IdP/email identity, NOT a
// channel transport kind) is DELIVERED via users.lookupByEmail rather than
// skipped as "non-Slack". This is the exact shape the runner builds for a
// user_preference_confirm card (host_approval.go currentTurnAuthorIdentity):
// the runner cannot know the delivering channel's kind, so it addresses the
// requester by their channel-agnostic IdP email and expects whatever channel
// delivers to resolve it. The Slack sender's per-recipient kind gate must not
// reject "idp" before the email lookup that CAN reach the user.
//
// The live symptom this guards: a user DM'd a Slack-output cron session
// (reviewbot) to change a personal preference; the confirm card's idp requester
// was skipped, delivered=false, and reportUndeliverable posted "couldn't reach
// anyone" into the monitoring channel — for a user who was standing right there
// in Slack.
func TestInteractionSender_Request_RequesterIdPKind_ResolvesViaEmail(t *testing.T) {
	const (
		email    = "erin@example.com"
		slackID  = "U_ERIN"
		chanID   = "C_IDP"
		threadTS = "1700000000.000009"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c, delivery: newInteractionDeliveryStore()}

	// The exact ExternalIdentity currentTurnAuthorIdentity builds for a
	// user_preference_confirm: Kind "idp" + Email + ExternalID=email, no Subject.
	p := channelevents.InteractionRequestPayload{
		Category:   categories.UserPreferenceConfirm,
		RequestRef: "req-idp-1",
		Lead:       `Save "notifications: off" as your default for this agent?`,
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Confirm", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStylePrimary},
			{ID: "deny", Label: "Cancel", Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope: channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{
				Kind:       "idp",
				Email:      identity.Email(email),
				ExternalID: identity.RawExternalID(email),
			},
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	require.Len(t, c.postEphemeralCalls, 1,
		"an idp (email) requester must be delivered via email lookup, not skipped as non-Slack")
	assert.Equal(t, slackID, c.postEphemeralCalls[0].userID,
		"must resolve the idp email to the Slack user id via GetUserByEmailContext")
	require.Len(t, c.lookupByEmailCalls, 1, "exactly one email lookup")
	assert.Equal(t, email, c.lookupByEmailCalls[0], "the lookup must use the idp identity's email")
	assert.Empty(t, c.postMessageCalls,
		"nothing was undeliverable, so no 'couldn't reach anyone' notice must be posted")
}

// TestInteractionSender_Request_QueuedMessages_RequesterResolvesAndDelivers
// is the Slack-boundary test: it carries the EXACT
// interaction_request(queued_messages) shape pipeline.go's Deliver builds
// (Category: queued_messages, Audience.Requester = toEnvelopeIdentity(the
// inbound sender) — raw ExternalID + verified Email, no Subject — and a
// single ActionKindDecision "interrupt" action) through the generic
// interactionSender's sendRequest, into a fake Slack client, and asserts the
// requester's ephemeral is actually delivered end to end.
//
// This is the raw-vs-canonical seam a real bug slipped through: unit
// tests that feed the renderer an already-canonical form never exercise the
// resolveSlackUserIDFromCanonical(Principal().AllowSynthetic().Canonical())
// round-trip a real pipeline-built payload requires. The ExternalID here is
// deliberately NOT valid base64-RawURL (mirrors
// TestInteractionSender_Request_RequesterRawIDWithEmail_ResolvesViaDerivedCanonical)
// so a regression to passing the raw ExternalID straight through fails loud
// (decode error) instead of coincidentally resolving.
func TestInteractionSender_Request_QueuedMessages_RequesterResolvesAndDelivers(t *testing.T) {
	const (
		rawID    = "Uqueued!not-valid-base64" // "!" is outside the base64-RawURL alphabet
		email    = "dana@example.com"
		slackID  = "U_DANA"
		chanID   = "C_QUEUED"
		threadTS = "1700000000.000004"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c}

	// Mirrors pipeline.go's Deliver: reqIdentity := toEnvelopeIdentity(ev.ExternalIDs).
	reqIdentity := channelevents.ExternalIdentity{
		Kind:       "slack",
		ExternalID: identity.RawExternalID(rawID),
		Email:      identity.Email(email),
	}
	p := channelevents.InteractionRequestPayload{
		Category:   categories.QueuedMessages,
		RequestRef: "req-queued-1",
		Lead:       "You messaged while I'm working — your message is queued.",
		Actions: []channelevents.InteractionAction{
			{ID: "interrupt", Label: "Interrupt & Send Now", Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &reqIdentity,
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send must succeed: the email must win the canonical derivation, not the raw ExternalID")

	require.Len(t, c.postEphemeralCalls, 1, "one ephemeral delivered to the requester")
	assert.Equal(t, chanID, c.postEphemeralCalls[0].channelID, "PostEphemeral channelID")
	assert.Equal(t, slackID, c.postEphemeralCalls[0].userID,
		"must resolve via the email-derived canonical (GetUserByEmailContext), never by decoding the raw ExternalID")
	require.Len(t, c.lookupByEmailCalls, 1, "exactly one email lookup")
	assert.Equal(t, email, c.lookupByEmailCalls[0], "the email lookup must use the verified Email, not anything derived from the raw ExternalID")
}

// TestInteractionSender_Request_DMFallbackWhenNoChannelContext verifies that
// a session with no channel_id gets a DM (OpenConversation + PostMessage)
// rather than an ephemeral.
func TestInteractionSender_Request_DMFallbackWhenNoChannelContext(t *testing.T) {
	const (
		email   = "bob@example.com"
		slackID = "U_BOB"
		linkURL = "https://identityd.example.com/link?tok=def456"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c}

	pl := credentialLinkRequestPayload(identity.RawExternalID(slackID), email, linkURL)
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, pl)

	_, err := s.Send(context.Background(), sessionDMOnly(), env)
	require.NoError(t, err, "Send")

	assert.Empty(t, c.postEphemeralCalls, "PostEphemeral must not be called for DM path")
	require.Len(t, c.openConvCalls, 1, "expected 1 OpenConversation call")
	assert.Equal(t, slackID, c.openConvCalls[0])
	require.Len(t, c.postMessageCalls, 1, "expected 1 PostMessage call")
	assert.Equal(t, "D"+slackID, c.postMessageCalls[0].channelID)
}

// TestInteractionSender_Request_NonSlackRequester_Skips verifies that a
// requester addressed to a non-Slack identity is skipped (not an error) —
// nothing for this kind to deliver.
func TestInteractionSender_Request_NonSlackRequester_Skips(t *testing.T) {
	c := &fakeSlackClient{}
	s := &interactionSender{client: c}

	pl := credentialLinkRequestPayload("irrelevant", "irrelevant", "https://identityd.example.com/link")
	pl.Audience.Requester = &channelevents.ExternalIdentity{Kind: "email", ExternalID: "alice@example.com"}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, pl)

	_, err := s.Send(context.Background(), sessionWithChannel("C01", "ts"), env)
	require.NoError(t, err, "non-slack requester must not error")
	// No prompt is delivered — this kind cannot address that identity.
	assert.Empty(t, c.postEphemeralCalls, "no DM/ephemeral prompt for a non-slack identity")
	// But the session is now waiting on a prompt nobody received, so the
	// failure is announced in-thread rather than swallowed.
	assert.NotEmpty(t, c.postMessageCalls,
		"an undeliverable request must surface in-thread, not vanish")
}

// TestInteractionSender_Request_ApproversScope_EmptyApprovers_Errors verifies
// that an AudienceApprovers-scope request with no approvers errors: a category
// may address a request to Audience.Approvers rather than Audience.Requester,
// but an empty approver list has nobody to deliver to and must fail loud rather
// than silently deliver nothing.
func TestInteractionSender_Request_ApproversScope_EmptyApprovers_Errors(t *testing.T) {
	c := &fakeSlackClient{}
	s := &interactionSender{client: c}

	pl := credentialLinkRequestPayload("irrelevant", "irrelevant", "https://identityd.example.com/link")
	pl.Audience = channelevents.InteractionAudience{
		Scope:     channelevents.AudienceApprovers,
		Approvers: nil,
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, pl)

	_, err := s.Send(context.Background(), sessionWithChannel("C01", "ts"), env)
	require.Error(t, err, "approvers-scope audience with no approvers must error")
	assert.Contains(t, err.Error(), "approvers")
}

// TestSendRequestApproversScopeDMOnly verifies the approver-scope
// delivery path: an AudienceApprovers request for a DM-only
// category (permission_request) opens a DM with the approver and records the
// delivery ref keyed by RequestRef (the request carries decision actions, so
// deliveryEditable(p) is true — a later interaction_applied needs to find it
// to edit in place). Uses a temporary registry override (snapshotCategories)
// so the assertion is pinned to a known Surface regardless of the production
// permission_request row's current shape.
func TestSendRequestApproversScopeDMOnly(t *testing.T) {
	restore := snapshotCategories(t)
	defer restore()
	channelinteractions.Reset()
	channelinteractions.Register(channelinteractions.Category{
		Name: "permission_request", Tone: channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideOwner,
		Resurface: channelinteractions.ResurfaceNone, Surface: channelinteractions.SurfaceDMOnly,
	})

	const slackID = "UOWNER"
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

	p := channelevents.InteractionRequestPayload{
		Category: "permission_request", RequestRef: "r1", Lead: "Session join request",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope: channelevents.AudienceApprovers,
			// A raw, email-less slack ExternalID: the sender derives its
			// canonical via Principal().AllowSynthetic().Canonical(), which
			// produces the same base64(slack::<id>) form
			// resolveSlackUserIDFromCanonical decodes without any Slack API
			// call — no lookup seeding needed.
			Approvers: []channelevents.ExternalIdentity{{Kind: "slack", ExternalID: identity.RawExternalID(slackID)}},
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)

	_, err := s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, env)
	require.NoError(t, err, "Send")
	assert.True(t, fc.openedDM, "DM-only approver prompt opens a DM")

	prompt, _, ok := s.delivery.get("r1")
	require.True(t, ok, "delivery recorded")
	assert.NotEmpty(t, prompt.TS)
}

// TestSendRequestApproversAllNonSlackRecipientsRecordsNothing verifies the
// fix for the phantom-delivery-record finding: an AudienceApprovers request
// whose sole approver is a non-Slack identity is skipped entirely (the
// existing non-slack-recipient behavior — logged, not an error), and because
// nothing was actually delivered, sendRequest must NOT record a delivery ref
// for the request even though the payload carries a decision action (so
// deliveryEditable(p) is true). Recording the zero-value deliveryRef{} anyway
// leaks a phantom entry for a request nothing ever delivered.
func TestSendRequestApproversAllNonSlackRecipientsRecordsNothing(t *testing.T) {
	restore := snapshotCategories(t)
	defer restore()
	channelinteractions.Reset()
	channelinteractions.Register(channelinteractions.Category{
		Name: "permission_request", Tone: channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideOwner,
		Resurface: channelinteractions.ResurfaceNone, Surface: channelinteractions.SurfaceDMOnly,
	})

	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

	p := channelevents.InteractionRequestPayload{
		Category: "permission_request", RequestRef: "r1", Lead: "Session join request",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: "email", ExternalID: "alice@example.com"}},
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)

	_, err := s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, env)
	require.NoError(t, err, "non-slack-only approvers must not error")
	assert.False(t, fc.openedDM, "no DM opened when every recipient is non-Slack")
	assert.Empty(t, fc.postEphemeralCalls)
	assert.Empty(t, fc.postMessageCalls)

	_, _, ok := s.delivery.get("r1")
	assert.False(t, ok, "no delivery record for a request that delivered to nobody")
}

// TestSendRequestPublicNoteNotPostedWhenNoRecipientDelivered guards against an
// orphaned public note: an AudienceApprovers request with Audience.PublicNote
// set, a channel context present, but whose sole approver is a non-Slack
// identity (so `delivered` stays false — nobody was actually asked) must not
// post the public note either. Posting unconditionally on p.Audience.PublicNote
// strands an "Approval pending" message forever, because the delivery record
// the applied-edit path needs to find the note is only written when delivered.
func TestSendRequestPublicNoteNotPostedWhenNoRecipientDelivered(t *testing.T) {
	restore := snapshotCategories(t)
	defer restore()
	channelinteractions.Reset()
	channelinteractions.Register(channelinteractions.Category{
		Name: "permission_request", Tone: channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideOwner,
		Resurface: channelinteractions.ResurfaceNone, Surface: channelinteractions.SurfaceDMOnly,
	})

	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

	p := channelevents.InteractionRequestPayload{
		Category: "permission_request", RequestRef: "r1", Lead: "Session join request",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:          channelevents.AudienceApprovers,
			Approvers:      []channelevents.ExternalIdentity{{Kind: "email", ExternalID: "alice@example.com"}},
			PublicNote:     true,
			PublicNoteBody: "Awaiting approval.",
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)
	sess := sessionWithChannel("CHAN", "")

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	// Reaching nobody is a FAILURE, not a benign skip: the session parks
	// awaiting a decision no one was asked for. It must be announced in-thread
	// so whoever is waiting learns the wait can never end.
	assert.True(t, fc.postedToChannel,
		"an undeliverable request must post an in-thread failure notice")

	// The "Approval pending" record is still withheld: nothing is pending,
	// so a later interaction_applied has no prompt to edit.
	_, _, ok := s.delivery.get("r1")
	assert.False(t, ok, "no delivery record for a request that delivered to nobody")
}

// TestInteractionSender_Request_ParticipantsScope_BroadcastsPlainToChannel
// verifies the new AudienceParticipants delivery mode (channel-broadcast,
// the plumbing provider_error_retry's addressee-less Retry prompt needs):
// sendRequest posts EXACTLY ONE plain (non-ephemeral, non-DM) channel
// message carrying the request blocks, and records the delivery ref under
// RequestRef so sendDecisionApplied can UpdateMessageContext it later. No
// PostEphemeral / OpenConversation call is made -- there is no addressee to
// resolve a recipient for.
func TestInteractionSender_Request_ParticipantsScope_BroadcastsPlainToChannel(t *testing.T) {
	const (
		chanID   = "CBROADCAST"
		threadTS = "1700000000.000009"
	)
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

	p := channelevents.InteractionRequestPayload{
		Category: "provider_error_retry", RequestRef: "r1", Lead: "Provider error -- retry?",
		Actions: []channelevents.InteractionAction{
			{ID: "retry", Label: "Retry", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	require.Len(t, fc.postMessageCalls, 1, "exactly one plain channel post for the broadcast")
	assert.Empty(t, fc.postEphemeralCalls, "broadcast must never post ephemeral (no addressee)")
	assert.Empty(t, fc.openConvCalls, "broadcast must never open a DM (no addressee)")
	assert.False(t, fc.openedDM, "broadcast must never open a DM (no addressee)")

	got := fc.postMessageCalls[0]
	assert.Equal(t, chanID, got.channelID, "broadcast posts to the session channel")
	_, vals, err := slackapi.UnsafeApplyMsgOptions("test-token", chanID, "http://test.invalid/", got.options...)
	require.NoError(t, err, "UnsafeApplyMsgOptions")
	assert.Equal(t, threadTS, vals.Get("thread_ts"), "broadcast must carry thread_ts so it lands in the session thread")

	prompt, _, ok := s.delivery.get("r1")
	require.True(t, ok, "broadcast with a decision action must record a delivery ref (deliveryEditable)")
	assert.Equal(t, chanID, prompt.ChannelID, "recorded delivery ref must carry the broadcast channel")
	assert.NotEmpty(t, prompt.TS, "recorded delivery ref must carry the broadcast ts")
}

// TestInteractionSender_Applied_Resolved_PostsConnectedNotice verifies that
// an interaction_applied{Outcome: resolved} for credential_link resolves
// the recipient from SessionInfo.SessionInitiator (the payload itself
// carries no audience) and posts a "connected" notice.
func TestInteractionSender_Applied_Resolved_PostsConnectedNotice(t *testing.T) {
	const (
		email    = "alice@example.com"
		slackID  = "U_ALICE"
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c}

	pl := channelevents.InteractionAppliedPayload{
		Category:    "credential_link",
		RequestRef:  "req-1",
		Outcome:     channelevents.OutcomeResolved,
		OutcomeText: "GitHub",
	}
	env := interactionEnvelope(t, channelevents.KindInteractionApplied, pl)
	sess := sessionWithChannel(chanID, threadTS)
	sess.SessionInitiator = identity.CanonicalFromTrusted(emailCanonical(email), "test fixture") // bare, no "user:" prefix

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	require.Len(t, c.postEphemeralCalls, 1, "expected 1 PostEphemeral call")
	got := c.postEphemeralCalls[0]
	assert.Equal(t, chanID, got.channelID)
	assert.Equal(t, slackID, got.userID)

	blocks := buildInteractionResolvedBlocks(pl)
	require.NotEmpty(t, interactionChildren(t, blocks), "the notice renders as a container")
	assert.Contains(t, concatBlockText(blocks), "GitHub connected")
	assert.Contains(t, concatBlockText(blocks), "large_green_circle",
		"the resolved tone chip replaces the hand-placed check mark")
}

// TestInteractionSender_Applied_Resolved_NoSessionInitiator_Skips verifies
// that a missing SessionInitiator is a graceful skip (best-effort UX notice,
// not a hard failure — the session resumes regardless of whether this
// notice lands).
func TestInteractionSender_Applied_Resolved_NoSessionInitiator_Skips(t *testing.T) {
	c := &fakeSlackClient{}
	s := &interactionSender{client: c}

	pl := channelevents.InteractionAppliedPayload{
		Category:   "credential_link",
		RequestRef: "req-1",
		Outcome:    channelevents.OutcomeResolved,
	}
	env := interactionEnvelope(t, channelevents.KindInteractionApplied, pl)
	sess := sessionWithChannel("C01CHAN", "ts") // SessionInitiator left empty

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "missing SessionInitiator must not error")
	assert.Empty(t, c.postEphemeralCalls)
}

// TestInteractionSender_Applied_DecisionCategory_ResponseRef_EditsInPlace
// verifies an identity_choice-shaped Applied (a decision-kind category, not
// credential_link) whose ResponseRef round-tripped from the click edits the
// SAME ephemeral in place via a response_url POST (replace_original:true),
// rather than posting a fresh message.
func TestInteractionSender_Applied_DecisionCategory_ResponseRef_EditsInPlace(t *testing.T) {
	bodies, srv := newResponseURLServer(t)
	defer srv.Close()

	s := &interactionSender{client: &fakeSlackClient{}, httpClient: responseURLClient(srv)}

	pl := channelevents.InteractionAppliedPayload{
		Category:    "identity_choice",
		RequestRef:  "idc-req-1",
		Outcome:     channelevents.OutcomeApproved,
		OutcomeText: "agent",
		ResponseRef: testResponseURL,
	}
	env := interactionEnvelope(t, channelevents.KindInteractionApplied, pl)

	_, err := s.Send(context.Background(), sessionWithChannel("C01CHAN", "ts"), env)
	require.NoError(t, err, "Send")

	got := bodies.values()
	require.Len(t, got, 1, "want one response_url POST")
	assert.Contains(t, got[0], `"replace_original":true`, "body sets replace_original")
	assert.Contains(t, got[0], "Running as the agent",
		"FU-3: the friendly label replaces the raw action id at render time (OutcomeText itself stays the raw id on the wire)")
}

// TestInteractionSender_Applied_DecisionCategory_NoResponseRef_NotifiesSessionInitiator
// verifies that a decision-kind Applied with no ResponseRef (resolved
// without a click — identity_choice's timeout path) falls back to a
// best-effort notice delivered to the session initiator, generalizing
// credential_link's out-of-band delivery.
func TestInteractionSender_Applied_DecisionCategory_NoResponseRef_NotifiesSessionInitiator(t *testing.T) {
	const (
		email    = "alice@example.com"
		slackID  = "U_ALICE"
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c}

	pl := channelevents.InteractionAppliedPayload{
		Category:   "identity_choice",
		RequestRef: "idc-req-2",
		Outcome:    channelevents.OutcomeExpired,
	}
	env := interactionEnvelope(t, channelevents.KindInteractionApplied, pl)
	sess := sessionWithChannel(chanID, threadTS)
	sess.SessionInitiator = identity.CanonicalFromTrusted(emailCanonical(email), "test fixture") // bare, no "user:" prefix

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	require.Len(t, c.postEphemeralCalls, 1, "expected 1 PostEphemeral call")
	got := c.postEphemeralCalls[0]
	assert.Equal(t, chanID, got.channelID)
	assert.Equal(t, slackID, got.userID)
}

// TestInteractionSender_Applied_DecisionCategory_NoResponseRefNoInitiator_Skips
// verifies the missing-both case is a graceful no-op, not an error — mirrors
// credential_link's missing-SessionInitiator behavior.
func TestInteractionSender_Applied_DecisionCategory_NoResponseRefNoInitiator_Skips(t *testing.T) {
	c := &fakeSlackClient{}
	s := &interactionSender{client: c}

	pl := channelevents.InteractionAppliedPayload{
		Category:   "identity_choice",
		RequestRef: "idc-req-3",
		Outcome:    channelevents.OutcomeDenied,
	}
	env := interactionEnvelope(t, channelevents.KindInteractionApplied, pl)
	sess := sessionWithChannel("C01CHAN", "ts") // SessionInitiator left empty

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "missing response_url and session initiator must not error")
	assert.Empty(t, c.postEphemeralCalls)
}

// TestDecisionAppliedEditsPromptAndPublicNote verifies the dual-surface edit:
// a decision-kind Applied with no ResponseRef (the click's response_url didn't
// round-trip, or the resolution didn't come from a click at all) edits BOTH the
// recorded prompt ref and the recorded public-note ref in place via
// UpdateMessageContext, then drops the delivery record — the interaction is
// resolved, so the in-process store must not grow unbounded.
func TestDecisionAppliedEditsPromptAndPublicNote(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}
	// Simulate a prior request that recorded both refs.
	s.delivery.record("r1", deliveryRef{ChannelID: "DOWNER", TS: "1.1"}, deliveryRef{ChannelID: "CHAN", TS: "2.2"})

	p := channelevents.InteractionAppliedPayload{
		Category: "permission_request", RequestRef: "r1",
		Outcome: channelevents.OutcomeApproved, OutcomeText: "approved",
		// no ResponseRef → edit via recorded prompt ref
	}
	env := interactionEnvelope(t, channelevents.KindInteractionApplied, p)
	_, err := s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, env)
	require.NoError(t, err, "Send")
	assert.Contains(t, fc.updatedRefs, "DOWNER:1.1", "prompt edited in place")
	assert.Contains(t, fc.updatedRefs, "CHAN:2.2", "public note edited to outcome")
	_, _, ok := s.delivery.get("r1")
	assert.False(t, ok, "delivery record dropped once the interaction resolved")
}

// TestDecisionAppliedResponseURLFailureStillEditsNoteAndDrops verifies the
// finding this task closes: a decision-kind Applied whose ResponseRef POST
// fails (expired/5xx response_url) must NOT early-return — the interaction
// is already resolved (the pipe published the Applied), so a cosmetic
// surface-edit failure must not skip the rest of the resolution bookkeeping.
// Send must still return no error, the recorded prompt ref must be edited as
// the fallback surface, the recorded public note must be edited to the
// outcome line, and the delivery record must be dropped — otherwise the
// public note is stuck "Approval pending" forever AND the delivery record
// leaks, growing the in-process store without bound.
func TestDecisionAppliedResponseURLFailureStillEditsNoteAndDrops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, httpClient: responseURLClient(srv), delivery: newInteractionDeliveryStore()}
	// Simulate a prior request that recorded both refs.
	s.delivery.record("r1", deliveryRef{ChannelID: "DOWNER", TS: "1.1"}, deliveryRef{ChannelID: "CHAN", TS: "2.2"})

	p := channelevents.InteractionAppliedPayload{
		Category: "permission_request", RequestRef: "r1",
		Outcome: channelevents.OutcomeApproved, OutcomeText: "approved",
		// ResponseRef set to the failing server → post_to_response_url errors,
		// must fall through rather than abort.
		ResponseRef: testResponseURL,
	}
	env := interactionEnvelope(t, channelevents.KindInteractionApplied, p)
	_, err := s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, env)
	require.NoError(t, err, "a failed response_url POST must be best-effort, not fatal")
	assert.Contains(t, fc.updatedRefs, "DOWNER:1.1", "recorded prompt ref edited as the fallback surface")
	assert.Contains(t, fc.updatedRefs, "CHAN:2.2", "public note still edited to the outcome despite the response_url failure")
	_, _, ok := s.delivery.get("r1")
	assert.False(t, ok, "delivery record still dropped despite the response_url failure")
}

// TestInteractionSender_Applied_CredentialLink_WrongOutcome_Errors verifies
// that the credential_link category itself errors on an outcome other than
// "resolved" — the category check alone isn't enough to pass.
func TestInteractionSender_Applied_CredentialLink_WrongOutcome_Errors(t *testing.T) {
	c := &fakeSlackClient{}
	s := &interactionSender{client: c}

	pl := channelevents.InteractionAppliedPayload{
		Category:   "credential_link",
		RequestRef: "req-3",
		Outcome:    channelevents.OutcomeExpired,
	}
	env := interactionEnvelope(t, channelevents.KindInteractionApplied, pl)
	sess := sessionWithChannel("C01CHAN", "ts")
	sess.SessionInitiator = identity.CanonicalFromTrusted(emailCanonical("alice@example.com"), "test fixture") // bare, no "user:" prefix

	_, err := s.Send(context.Background(), sess, env)
	require.Error(t, err, "credential_link with a non-resolved outcome must error")
	assert.Contains(t, err.Error(), "resolved")
	assert.Empty(t, c.postEphemeralCalls)
}

// TestInteractionSender_UnexpectedEnvelopeKind_Errors verifies that an
// envelope whose kind is neither interaction_request nor interaction_applied
// returns an error — fail-loud, matching every sibling sub-channel sender.
func TestInteractionSender_UnexpectedEnvelopeKind_Errors(t *testing.T) {
	c := &fakeSlackClient{}
	s := &interactionSender{client: c}
	env := channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindUserMessage,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "sess-1"},
		PublishedAt: time.Now().UTC(),
		Payload:     []byte(`{}`),
	}
	_, err := s.Send(context.Background(), sessionWithChannel("C01", "ts"), env)
	require.Error(t, err, "expected error for unexpected kind")
	assert.Contains(t, err.Error(), "unexpected kind")
	assert.Empty(t, c.postEphemeralCalls)
}

// TestInteractionSender_NilClient verifies a nil Slack client (misconfigured
// secret) errors without panicking.
func TestInteractionSender_NilClient(t *testing.T) {
	s := &interactionSender{client: nil}
	pl := credentialLinkRequestPayload("irrelevant", "irrelevant", "https://identityd.example.com/link")
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, pl)
	_, err := s.Send(context.Background(), sessionWithChannel("C01", "ts"), env)
	require.Error(t, err, "expected error for nil client")
	assert.Contains(t, err.Error(), "unconfigured")
}

// TestResolveFieldMentions_ResolvesToSlackMentionsWithFallback pins the
// info_leakage "Would share with" mentions follow-up: a Field carrying
// Mentions renders each resolvable recipient as a Slack "<@id>" mention, and
// a recipient this fake client can't resolve (no seeded email) falls back to
// its own display text — the recipient is NEVER dropped, matching
// info_leakage's "the data owner must see exactly who" safety invariant. A
// Field with no Mentions passes through with its Value untouched.
func TestResolveFieldMentions_ResolvesToSlackMentionsWithFallback(t *testing.T) {
	const (
		resolvableEmail = "evan@corp.example"
		resolvableID    = "U_EVAN"
		missingEmail    = "dana@corp.example" // not seeded on the fake client
	)
	c := fakeClientWithUser(resolvableEmail, resolvableID)

	fields := []channelevents.InteractionField{
		{
			Label: "Would share with",
			Value: resolvableEmail + ", " + missingEmail,
			Mentions: []channelevents.ExternalIdentity{
				{Kind: "slack", Subject: identity.Subject("user:" + emailCanonical(resolvableEmail))},
				{Kind: "slack", Subject: identity.Subject("user:" + emailCanonical(missingEmail))},
			},
		},
		{Label: "Other", Value: "unchanged"},
	}

	got := resolveFieldMentions(context.Background(), c, fields, logr.Discard(), "req-mentions-1")

	require.Len(t, got, 2)
	assert.Equal(t, "<@"+resolvableID+">, "+missingEmail, got[0].Value,
		"resolvable recipient renders as a Slack mention; the resolution miss falls back to its own display text")
	assert.Equal(t, "unchanged", got[1].Value, "a field with no Mentions must pass through untouched")

	require.Len(t, c.lookupByEmailCalls, 2)
	assert.Contains(t, c.lookupByEmailCalls, resolvableEmail)
	assert.Contains(t, c.lookupByEmailCalls, missingEmail)
}

// TestResolveFieldMentions_NonSlackMention_FallsBackWithoutAPICall verifies
// that a Mention addressed to a non-Slack identity is never handed to the
// Slack API — it falls back to display text immediately, same as a
// resolution error.
func TestResolveFieldMentions_NonSlackMention_FallsBackWithoutAPICall(t *testing.T) {
	c := &fakeSlackClient{}
	fields := []channelevents.InteractionField{
		{
			Label:    "Would share with",
			Value:    "someone@example.com",
			Mentions: []channelevents.ExternalIdentity{{Kind: "email", ExternalID: "someone@example.com"}},
		},
	}

	got := resolveFieldMentions(context.Background(), c, fields, logr.Discard(), "req-mentions-2")

	require.Len(t, got, 1)
	assert.Equal(t, "someone@example.com", got[0].Value)
	assert.Empty(t, c.lookupByEmailCalls, "a non-slack mention must never reach the Slack API")
}

// TestInteractionSender_Request_InfoLeakage_FieldMentionsDeliverEndToEnd
// exercises the same resolution through the full sendRequest path (not just
// the pure resolveFieldMentions helper): an info_leakage-shaped request whose
// "Would share with" Field carries Mentions still delivers successfully to
// its (unrelated) approver — wiring the resolution step into sendRequest
// must not break or block normal delivery.
func TestInteractionSender_Request_InfoLeakage_FieldMentionsDeliverEndToEnd(t *testing.T) {
	const (
		ownerEmail = "owner@corp.example"
		ownerID    = "U_OWNER"
		recipEmail = "evan@corp.example"
		recipID    = "U_EVAN"
		chanID     = "C_LEAK"
		threadTS   = "1700000000.000010"
	)
	c := fakeClientWithUser(ownerEmail, ownerID)
	c.lookupByEmail[recipEmail] = &slackapi.User{ID: recipID}
	s := &interactionSender{client: c, delivery: newInteractionDeliveryStore()}

	p := channelevents.InteractionRequestPayload{
		Category:   "info_leakage",
		RequestRef: "req-leak-1",
		Lead:       "Share request",
		Fields: []channelevents.InteractionField{
			{
				Label: "Would share with",
				Value: recipEmail,
				Mentions: []channelevents.ExternalIdentity{
					{Kind: "slack", Subject: identity.Subject("user:" + emailCanonical(recipEmail))},
				},
			},
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: "slack", Subject: identity.Subject("user:" + emailCanonical(ownerEmail))}},
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send must succeed: field-mention resolution must not break normal delivery")

	require.Len(t, c.postEphemeralCalls, 1, "the approver still receives the prompt")
	assert.Equal(t, ownerID, c.postEphemeralCalls[0].userID)
}

// TestBuildInteractionRequestBlocks verifies the pure block builder: link
// actions render as URL buttons; decision actions render as buttons whose
// value encodes the generic interaction discriminator.
func TestBuildInteractionRequestBlocks(t *testing.T) {
	pl := channelevents.InteractionRequestPayload{
		Category:   "some_category",
		RequestRef: "req-mixed",
		Lead:       "Connect your accounts",
		Body:       "Needed for the task.",
		Actions: []channelevents.InteractionAction{
			{ID: "connect", Label: "Connect", Kind: channelevents.ActionKindLink, URL: "https://example.com/link", Style: channelevents.ActionStylePrimary},
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
		},
	}
	blocks := buildInteractionRequestBlocks(pl, "default/sess-1")
	actions := interactionActionBlock(t, blocks)
	require.NotNil(t, actions, "the container must carry an action row")
	require.Len(t, actions.Elements.ElementSet, 2, "both the link and the decision action must render as buttons")

	linkBtn, ok := actions.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	require.True(t, ok)
	assert.Equal(t, "https://example.com/link", linkBtn.URL)

	decisionBtn, ok := actions.Elements.ElementSet[1].(*slackapi.ButtonBlockElement)
	require.True(t, ok)
	assert.Empty(t, decisionBtn.URL, "decision action must not carry a URL")
	assert.Equal(t, "approve", decisionBtn.ActionID)
	decoded, ok := decodeApprovalButtonValue(decisionBtn.Value)
	require.True(t, ok, "decision button value must decode")
	assert.Equal(t, discInteraction, decoded.V)
	assert.Equal(t, "req-mixed", decoded.R, "requestRef")
	assert.Equal(t, "approve", decoded.D, "actionId")
	assert.Equal(t, "some_category", decoded.C, "category")
	assert.Equal(t, "default/sess-1", decoded.S, "sessRef")
}

// TestBuildInteractionRequestBlocks_IdentityChoice_RendersThreeDecisionButtons
// verifies identity_choice's exact 3-way action shape
// (agent/userPassthrough/cancel, pkg/agent/runner/identitygate.go) renders
// as 3 Block-Kit buttons, each carrying the category so the click handler
// can build a valid InteractionDecisionPayload.
func TestBuildInteractionRequestBlocks_IdentityChoice_RendersThreeDecisionButtons(t *testing.T) {
	pl := channelevents.InteractionRequestPayload{
		Category:   "identity_choice",
		RequestRef: "idc-req-1",
		Lead:       "Which identity should this agent use for this session?",
		Actions: []channelevents.InteractionAction{
			{ID: "agent", Kind: channelevents.ActionKindDecision, Label: "Run as the agent"},
			{ID: "userPassthrough", Kind: channelevents.ActionKindDecision, Label: "Run as me"},
			{ID: "cancel", Kind: channelevents.ActionKindDecision, Label: "Cancel"},
		},
	}
	blocks := buildInteractionRequestBlocks(pl, "default/sess-idc")
	actions := interactionActionBlock(t, blocks)
	require.NotNil(t, actions, "the container must carry an action row")
	require.Len(t, actions.Elements.ElementSet, 3, "one button per decision action")

	wantIDs := []string{"agent", "userPassthrough", "cancel"}
	for i, el := range actions.Elements.ElementSet {
		btn, ok := el.(*slackapi.ButtonBlockElement)
		require.True(t, ok)
		assert.Equal(t, wantIDs[i], btn.ActionID)
		decoded, ok := decodeApprovalButtonValue(btn.Value)
		require.True(t, ok)
		assert.Equal(t, discInteraction, decoded.V)
		assert.Equal(t, "idc-req-1", decoded.R)
		assert.Equal(t, wantIDs[i], decoded.D)
		assert.Equal(t, "identity_choice", decoded.C)
		assert.Equal(t, "default/sess-idc", decoded.S)
	}
}

// TestBuildInteractionRequestBlocks_NoActions_OmitsActionsBlock verifies
// that a payload with no actions (a read-only notice) renders section-only
// — no empty actions block.
func TestBuildInteractionRequestBlocks_NoActions_OmitsActionsBlock(t *testing.T) {
	pl := channelevents.InteractionRequestPayload{Lead: "FYI"}
	blocks := buildInteractionRequestBlocks(pl, "default/sess-1")
	assert.Nil(t, interactionActionBlock(t, blocks), "no Details ⇒ no action row")
}

// Slack has no colour or glyph affordance, so the one tier that MUST survive
// — external, the reason a card is worth reading — has to reach the message
// as a word. The raw handle carried as Hint has no on-demand affordance on
// this surface either (no hover, no tooltip), so it must never appear at all,
// not even for the line it names.
func TestBuildInteractionRequestBlocks_StructuredFieldMarksExternalAsTextAndOmitsHandle(t *testing.T) {
	pl := channelevents.InteractionRequestPayload{
		Category:   "plan_phase",
		RequestRef: "req-1",
		Lead:       "Plan approval",
		Fields: []channelevents.InteractionField{
			{
				Label: "What",
				Value: "perm:read:git_repo\nperm:write:tracker_issue\nperm:push:git_repo",
				Items: []channelevents.InteractionItem{
					{Text: "Phase 1", Items: []channelevents.InteractionItem{
						{Text: "Read the repository", Tone: channelevents.ToneReadonly, Hint: "perm:read:git_repo"},
						{Text: "Update the issue", Tone: channelevents.ToneReadwrite, Hint: "perm:write:tracker_issue"},
						{
							Text: "Push commits", Tone: channelevents.ToneExternal,
							Hint: "perm:push:git_repo", Detail: "leaves this session",
						},
					}},
				},
			},
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
		},
	}

	blocks := buildInteractionRequestBlocks(pl, "default/demo-session")
	text := interactionText(t, blocks)

	assert.Contains(t, text, "Push commits")
	assert.Contains(t, text, "external", "colour has no equivalent in Slack, so the tier must reach as text")
	assert.Contains(t, text, "leaves this session")
	assert.NotContains(t, text, "perm:push:git_repo",
		"the raw handle must never appear — Slack has no on-demand affordance to reveal it")
	assert.NotContains(t, text, "perm:read:git_repo", "same for a non-external line's handle")
	assert.NotContains(t, text, "perm:write:tracker_issue", "same for the readwrite line's handle")

	// Text is Slack's ONLY channel for tier — no colour, no glyph — so all
	// three tiers must reach it distinguishably, not just external. A reader
	// who cannot tell a read from a write from the unmarked line has been
	// given exactly the ambiguity this feature exists to remove.
	require.True(t, strings.Contains(text, "read:") || strings.Contains(text, "read only"),
		"readonly must carry its own marker, not render identical to an untiered line")
	assert.Contains(t, text, "write:", "readwrite must carry its own marker, distinct from readonly")
	assert.Contains(t, text, "external:", "external must carry its own marker, distinct from the other two")

	// external is the one tier that cannot be undone and must stay the most
	// prominent of the three — bold, where the other two are not.
	assert.Contains(t, text, "*external:*", "external's marker must be bold, the heaviest of the three")
	assert.NotContains(t, text, "*read:*", "readonly must not be bolded to external's weight")
	assert.NotContains(t, text, "*write:*", "readwrite must not be bolded to external's weight")
}

// TestInteractionOutcomeText_IdentityChoiceUsesFriendlyLabel is the focused
// unit test for FU-3: interactionOutcomeText swaps identity_choice's raw
// action id (OutcomeText) for the friendly label at render time, without
// touching the payload itself — OutcomeText stays load-bearing for
// internal/cmd/runner's subscribeInteractionApplied. Every other category's
// OutcomeText (e.g. credential_link's credential name) passes through
// unchanged, and an unrecognized identity_choice action id degrades to the
// raw text rather than a blank string.
func TestInteractionOutcomeText_IdentityChoiceUsesFriendlyLabel(t *testing.T) {
	cases := []struct {
		name string
		pl   channelevents.InteractionAppliedPayload
		want string
	}{
		{
			name: "identity_choice agent -> friendly label",
			pl:   channelevents.InteractionAppliedPayload{Category: "identity_choice", Outcome: channelevents.OutcomeApproved, OutcomeText: "agent"},
			want: ":large_green_circle: Running as the agent",
		},
		{
			name: "identity_choice userPassthrough -> friendly label",
			pl:   channelevents.InteractionAppliedPayload{Category: "identity_choice", Outcome: channelevents.OutcomeApproved, OutcomeText: "userPassthrough"},
			want: ":large_green_circle: Running as you",
		},
		{
			name: "identity_choice cancel -> friendly label, denied icon",
			pl:   channelevents.InteractionAppliedPayload{Category: "identity_choice", Outcome: channelevents.OutcomeDenied, OutcomeText: "cancel"},
			want: ":large_orange_circle: Cancelled",
		},
		{
			name: "identity_choice unrecognized action id -> raw text passes through",
			pl:   channelevents.InteractionAppliedPayload{Category: "identity_choice", Outcome: channelevents.OutcomeApproved, OutcomeText: "not-a-real-action"},
			want: ":large_green_circle: not-a-real-action",
		},
		{
			name: "credential_link OutcomeText (a credential name) is untouched by the identity_choice map",
			pl:   channelevents.InteractionAppliedPayload{Category: "credential_link", Outcome: channelevents.OutcomeResolved, OutcomeText: "GitHub"},
			want: ":large_green_circle: GitHub",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, interactionOutcomeText(tc.pl))
		})
	}
}

// TestSubChannelSender_InteractionReturnsNonNil verifies the kind.go wire-up:
// Kind.SubChannelSender("interaction", ...) returns a non-nil Sender.
func TestSubChannelSender_InteractionReturnsNonNil(t *testing.T) {
	k := &Kind{}
	sender := k.SubChannelSender("interaction", channelkinds.Deps{})
	require.NotNil(t, sender, `SubChannelSender("interaction") must return a non-nil Sender`)
}

// TestSubChannelSender_CredentialRequestNoLongerRegistered pins that the
// credential_request / credential_linked sub-channel names resolve to no
// sender: their publishers (pkg/channels/channelsd/pipeline/credential_request.go,
// credential_linked.go) publish interaction_request / interaction_applied, so
// "interaction" is the only sub-channel that renders credential_link prompts.
func TestSubChannelSender_CredentialRequestNoLongerRegistered(t *testing.T) {
	k := &Kind{}
	assert.Nil(t, k.SubChannelSender(string(channelevents.KindCredentialRequest), channelkinds.Deps{}))
	assert.Nil(t, k.SubChannelSender(string(channelevents.KindCredentialLinked), channelkinds.Deps{}))
}
