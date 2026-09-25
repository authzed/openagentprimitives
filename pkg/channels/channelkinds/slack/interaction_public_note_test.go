package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// registerToolApprovalCategory registers the one category that sets
// Audience.PublicNote today, and restores the real registry afterwards.
func registerToolApprovalCategory(t *testing.T) {
	t.Helper()
	restore := snapshotCategories(t)
	t.Cleanup(restore)
	channelinteractions.Reset()
	channelinteractions.Register(channelinteractions.Category{
		Name: "tool_approval", Tone: channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideOwner,
		Resurface: channelinteractions.ResurfaceNone, Surface: channelinteractions.SurfaceDMOnly,
	})
}

// publicNoteRequest is the shape host_approval.go publishes for a tool
// approval: an approver-scoped prompt whose PublicNoteBody is the summarizer
// LLM's one-liner over agent-controlled tool arguments (whatLine), wrapped in
// the agent's display name.
func publicNoteRequest(body string) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		Category: "tool_approval", RequestRef: "r-note", Lead: "Approval needed",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope: channelevents.AudienceApprovers,
			// A raw, email-less slack ExternalID resolves through
			// Principal().AllowSynthetic().Canonical() with no Slack API call.
			Approvers:      []channelevents.ExternalIdentity{{Kind: "slack", ExternalID: identity.RawExternalID("UOWNER")}},
			PublicNote:     true,
			PublicNoteBody: body,
		},
	}
}

// sendPublicNote delivers a public-note-bearing request and returns the two
// sinks the note lands in: the message's plain-text `text` field (the push
// preview, posted with escape=false so slack-go does NOT escape it) and its
// blocks (a MarkdownType section).
//
// The note is told apart from the recipient's DM prompt by its fixed lead.
// Both sinks are read because they are separate code paths — an escape applied
// to one and not the other is exactly the shape of the defect this file
// guards, and blocks are invisible to UnsafeApplyMsgOptions.
func sendPublicNote(t *testing.T, body string) (text, blocks string) {
	t.Helper()
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

	_, err := s.Send(context.Background(),
		sessionWithChannel("CHAN", ""),
		interactionEnvelope(t, channelevents.KindInteractionRequest, publicNoteRequest(body)))
	require.NoError(t, err, "Send")

	for _, call := range fc.postMessageCalls {
		_, vals, applyErr := slackapi.UnsafeApplyMsgOptions("test-token", call.channelID, "http://test.invalid/", call.options...)
		require.NoError(t, applyErr, "UnsafeApplyMsgOptions")
		if got := vals.Get("text"); strings.HasPrefix(got, publicNoteLead) {
			return got, unescapedBlocksJSON(t, call.options)
		}
	}
	require.Fail(t, "no PostMessageContext call carried the public note")
	return "", ""
}

// unescapedBlocksJSON is msgOptionBlocksJSON with Go's default HTML escaping
// undone. encoding/json writes `<` as `<`, so an assertion spelled in the
// characters Slack actually parses ("<@U123>", "<url|label>") would never match
// the raw document — and NotContains would pass vacuously, testing nothing.
func unescapedBlocksJSON(t *testing.T, opts []slackapi.MsgOption) string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(msgOptionBlocksJSON(t, opts)), &v), "decode blocks JSON")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(v), "re-encode blocks JSON without HTML escaping")
	return buf.String()
}

// TestPublicNote_PublisherBodyIsInertOnBothSinks is the guard for the highest-
// exposure publisher slot in this package: PublicNoteBody is the summarizer
// LLM's sentence over agent-controlled tool arguments, and its note is a PUBLIC
// channel post styled as the platform's own "Approval pending" card, one line
// above the real approver mentions. A forged `<url|label>` there is the
// forged-action threat inert.go exists to close, on the surface where it is
// seen by everyone rather than by one approver.
func TestPublicNote_PublisherBodyIsInertOnBothSinks(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		absent string
	}{
		{
			name:   "a forged link cannot render as a hyperlink in the public post",
			body:   "*demo-agent* wants to " + lureLink,
			absent: lureLink,
		},
		{
			name:   "a forged user mention cannot ping a bystander from the public post",
			body:   "*demo-agent* wants to notify <@U_VICTIM>",
			absent: "<@U_VICTIM>",
		},
		{
			name:   "a channel-wide ping cannot be smuggled through the note body",
			body:   "*demo-agent* wants to <!channel> get everyone's attention",
			absent: "<!channel>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registerToolApprovalCategory(t)
			text, blocks := sendPublicNote(t, tc.body)
			assert.NotContains(t, text, tc.absent,
				"the note's plain-text sink is posted with escape=false — slack-go does not escape it for us")
			assert.NotContains(t, blocks, tc.absent,
				"the note's block sink is a mrkdwn section")
		})
	}
}

// TestPublicNote_InjectedMarkupStaysVisible: inert is not deleted. A reader
// must still see what was attempted, or the escaping hides an attack instead
// of defusing it (the same bar interaction_inertness_test.go sets for the card).
func TestPublicNote_InjectedMarkupStaysVisible(t *testing.T) {
	registerToolApprovalCategory(t)
	text, blocks := sendPublicNote(t, "*demo-agent* wants to "+lureLink)
	assert.Contains(t, text, lureLinkVisible, "the attempt must render as literal characters a human can read")
	assert.Contains(t, blocks, lureLinkVisible, "...on the block sink as well as the plain-text one")
}

// TestPublicNote_ApproverMentionsStayLive is the false-positive guard, and the
// reason the body cannot simply be escaped after composition: approverMentions
// composes `<@U…>` that THIS kind resolved, and withApproverClause appends them
// to the same string. Escaping the composed note would render them
// "&lt;@UOWNER&gt;" and the channel would stop being told who it is waiting on.
func TestPublicNote_ApproverMentionsStayLive(t *testing.T) {
	registerToolApprovalCategory(t)
	text, blocks := sendPublicNote(t, "*demo-agent* wants to push to the release branch")
	assert.Contains(t, text, "<@UOWNER>", "the kind's own approver mention must stay clickable")
	assert.Contains(t, blocks, "<@UOWNER>")
	assert.NotContains(t, text, "&lt;@UOWNER&gt;", "escaping must run BEFORE the mentions are composed, not after")
}

// TestPublicNotePostedToChannel verifies postPublicNote posts to the session's
// channel and never a DM — the note is deliberately public.
//
// Inertness is deliberately NOT asserted at this seam: by the time a body
// reaches postPublicNote the kind's own live `<@U…>` mentions are already in
// the same string, so this function cannot escape it without killing them.
// publicNoteText is where the body is made inert, and the tests above are what
// pin it. Asserting inertness here instead would make the sink look covered
// while the strongest payloads walked through.
func TestPublicNotePostedToChannel(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

	ref, err := s.postPublicNote(context.Background(), sessionWithChannel("CHAN", ""), "r1",
		publicNoteText("awaiting approval", "<@UOWNER>"))
	require.NoError(t, err)
	assert.Equal(t, "CHAN", ref.ChannelID)
	assert.True(t, fc.postedToChannel, "public note goes to the channel, not a DM")
	assert.Contains(t, fc.lastPostedText, "<@UOWNER>", "the kind's approver mention rides through live")
}

// TestPublicNoteText_EscapesTheBodyBeforeComposingMentions pins the ORDER that
// makes publicNoteText correct, at the unit it belongs to. Reversing the two
// steps still compiles and still produces a plausible-looking note — it just
// silently swaps which half is live.
func TestPublicNoteText_EscapesTheBodyBeforeComposingMentions(t *testing.T) {
	got := publicNoteText("*demo-agent* wants to "+lureLink, "<@UOWNER>")

	assert.NotContains(t, got, lureLink, "the publisher's body must be inert")
	assert.Contains(t, got, lureLinkVisible,
		"...and still readable")
	assert.Contains(t, got, "Waiting on <@UOWNER>.", "the kind's own mention must stay live")
	assert.Contains(t, got, "*demo-agent*", "publisher bold is not a link/mention character and stays live")
}

// TestRunInteractionTicker_ReRendersTheNoteWithoutRelivingIt covers the note's
// THIRD sink pair. The ticker re-posts the same body through publicNoteBlocks
// and publicNoteNotifyText every 30s in production; it is a separate call site
// from postPublicNote, and a re-render that unescaped (or that escaped the
// whole composed string) would undo either half of publicNoteText's guarantee
// minutes after the note was posted correctly.
func TestRunInteractionTicker_ReRendersTheNoteWithoutRelivingIt(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		s.runInteractionTicker(ctx, deliveryRef{ChannelID: "C1", TS: "100.1"}, time.Now(),
			5*time.Millisecond, publicNoteText("*demo-agent* wants to "+lureLink, "<@UOWNER>"), logr.Discard())
		close(done)
	}()

	calls := waitForUpdate(t, fc, 2*time.Second)
	text := updateText(t, calls[0])
	blocks := unescapedBlocksJSON(t, calls[0].opts)

	assert.NotContains(t, text, lureLink, "the tick's plain-text sink must not re-liven the body")
	assert.NotContains(t, blocks, lureLink, "the tick's mrkdwn section must not re-liven the body")
	assert.Contains(t, text, "<@UOWNER>", "the tick must not escape the mention the note already carries")
	assert.Contains(t, blocks, "<@UOWNER>")

	cancel()
	waitClosed(t, done, 2*time.Second, "ctx-cancel")
}

// TestPublicNoteNoChannelContext_SkipsGracefully verifies that a session
// with no channel context — a nil Channel, or a DM-only Channel binding with
// no channel_id — is a graceful skip: no error, no post, and a zero-value
// deliveryRef. The note is deliberately public (never a DM), so when there
// is no channel to post it in there is nothing to do.
func TestPublicNoteNoChannelContext_SkipsGracefully(t *testing.T) {
	cases := []struct {
		name string
		sess channelkinds.SessionInfo
	}{
		{name: "nil Channel", sess: channelkinds.SessionInfo{Namespace: "default", Name: "s1"}},
		{name: "Channel with no channel_id (DM-only)", sess: sessionDMOnly()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeSlackClient{}
			s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

			ref, err := s.postPublicNote(context.Background(), tc.sess, "r1", "note body")
			require.NoError(t, err, "no channel context must not error")
			assert.Equal(t, deliveryRef{}, ref, "no channel context yields the zero-value deliveryRef")
			assert.Empty(t, fc.postMessageCalls, "no channel context must not post")
		})
	}
}

// TestPostPublicNoteError_Propagates verifies that a PostMessageContext
// failure on the channel post is wrapped and returned, not swallowed — the
// public note is best-effort only when there is no channel context to post
// to (the graceful-skip paths above); an actual API failure must surface.
func TestPostPublicNoteError_Propagates(t *testing.T) {
	fc := &fakeSlackClient{postMessageErr: errors.New("channel_not_found")}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}
	sess := sessionWithChannel("CHAN", "")

	_, err := s.postPublicNote(context.Background(), sess, "r1", "Awaiting approval.")
	require.Error(t, err, "postPublicNote must propagate a PostMessageContext failure")
	assert.Contains(t, err.Error(), "channel_not_found")
	assert.Contains(t, err.Error(), "r1", "wrapped error should carry the requestRef")
}

// TestSendRequestPostsPublicNoteAndRecordsInDelivery verifies sendRequest's
// wiring: when Audience.PublicNote is set, sendRequest posts the note to the
// channel (via postPublicNote) in addition to the recipient's prompt, and
// records the note's ref as the publicNote half of the delivery entry
// alongside the prompt ref — the applied-edit path needs both halves.
func TestSendRequestPostsPublicNoteAndRecordsInDelivery(t *testing.T) {
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
			Approvers:      []channelevents.ExternalIdentity{{Kind: "slack", ExternalID: identity.RawExternalID(slackID)}},
			PublicNote:     true,
			PublicNoteBody: "Awaiting approval.",
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)
	sess := sessionWithChannel("CHAN", "")

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")
	assert.True(t, fc.postedToChannel, "public note posts to the channel")

	prompt, publicNote, ok := s.delivery.get("r1")
	require.True(t, ok, "delivery recorded")
	assert.NotEmpty(t, prompt.TS, "prompt ref recorded")
	assert.Equal(t, "CHAN", publicNote.ChannelID, "public note ref recorded")
}

// TestSendRequest_JoinRequest_StructuralIdentitiesRenderLive is the render
// half of the join-request contract; the publish half is
// pkg/channels/channelsd/pipeline's
// TestHandlePermissionDeny_NamesIdentitiesStructurallyNotAsMarkup, which pins
// that the pipeline declares both parties structurally instead of
// interpolating channel-native mention markup into two fields every surface
// renders inert.
//
// The two halves are deliberately separate tests in separate packages, and
// this comment is the seam between them: neither package can see the other's
// end, and a per-package review of either one alone cannot tell that a
// structurally-declared identity actually reaches a reader as a live mention.
// That is what this asserts, on BOTH surfaces the one join request produces —
// the approver's private card (Field.Mentions → "<@…>") and the PUBLIC note
// (Audience.Approvers → the "Waiting on …" clause).
func TestSendRequest_JoinRequest_StructuralIdentitiesRenderLive(t *testing.T) {
	restore := snapshotCategories(t)
	t.Cleanup(restore)
	channelinteractions.Reset()
	channelinteractions.Register(channelinteractions.Category{
		Name: "permission_request", Tone: channelinteractions.ToneWaiting,
		Deciders:  channelinteractions.DecideOwner,
		Resurface: channelinteractions.ResurfaceNone, Surface: channelinteractions.SurfaceDMOnly,
	})

	const requesterEmail = "requester@corp.example"
	fc := fakeClientWithUser(requesterEmail, "U_REQ")
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

	p := channelevents.InteractionRequestPayload{
		Category: "permission_request", RequestRef: "r-join", Lead: "Session join request",
		Body: "🔔 Someone is asking to talk to your *demo-class* agent session.",
		// The requester, declared the way the pipeline declares them: full
		// structured identity in Mentions, display text in Value as the
		// fallback for a surface that resolves nothing.
		Fields: []channelevents.InteractionField{
			{Label: "From", Value: requesterEmail, Mentions: []channelevents.ExternalIdentity{
				{Kind: "slack", ExternalID: identity.RawExternalID("U_REQ"), Email: identity.Email(requesterEmail)},
			}},
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:          channelevents.AudienceApprovers,
			Approvers:      []channelevents.ExternalIdentity{{Kind: "slack", ExternalID: identity.RawExternalID("U_OWNER")}},
			PublicNote:     true,
			PublicNoteBody: "⏳ Approval needed — your request to join this session is awaiting approval.",
		},
	}
	_, err := s.Send(context.Background(), sessionWithChannel("CHAN", ""),
		interactionEnvelope(t, channelevents.KindInteractionRequest, p))
	require.NoError(t, err, "Send")

	var card, note string
	for _, call := range fc.postMessageCalls {
		blocks := unescapedBlocksJSON(t, call.options)
		if strings.Contains(blocks, publicNoteLead) {
			note = blocks
			continue
		}
		card = blocks
	}
	require.NotEmpty(t, card, "the approver's DM card must have been posted")
	require.NotEmpty(t, note, "the public note must have been posted")

	assert.Contains(t, card, "<@U_REQ>", "the requester's Field.Mentions must render as a live mention on the card")
	assert.NotContains(t, card, "&lt;@U_REQ&gt;", "...not as escaped literal text")
	assert.Contains(t, note, "Waiting on <@U_OWNER>.",
		"the approver comes from Audience.Approvers and the kind composes the clause itself")
	assert.NotContains(t, note, "&lt;@U_OWNER&gt;", "...live, because the kind composed it after the body was escaped")
}
