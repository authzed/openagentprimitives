// pkg/channels/channelkinds/slack/interaction_outcome_text_test.go
//
// The RESOLVED interaction's own two text slots — InteractionAppliedPayload's
// OutcomeText and Reason — and the four live mrkdwn sinks they reach.
//
// interaction_applied_test.go covers the cached REQUEST payload replayed onto
// the resolved card (escapePublisherPayload's sweep, a1a13c8b1). This file
// covers the other half of the same card: the fields that arrive on the
// APPLIED payload itself, which that sweep never sees because they are
// composed in afterwards.
//
// They are not merely publisher-authored. On the queued_messages path they are
// RUNNER-controlled: internal/cmd/channelsd/interrupt_applied_bridge.go builds
// OutcomeText as couldntInterruptText(ip.Reason) and copies Reason straight
// across, both off the runner's own .out.interrupt_applied publish. So an
// agent that puts a link span in the interrupt reason gets it rendered inside
// a card Slack presents as the platform's own resolution notice.
//
// The tests drive that real producer's payload shape through sender.Send
// rather than calling the builders directly, so they measure REACHABILITY —
// that the value actually crosses into a markup surface — and not merely that
// some helper escapes.
package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// interruptReasonLure is an interrupt reason shaped the way an agent could
// write one: a forged platform action plus a channel-wide ping. The runner
// controls this string end to end — it rides .out.interrupt_applied's Reason.
const interruptReasonLure = "the tool is still running " + lureLink + " <!channel>"

// appliedFromInterruptBridge builds the InteractionAppliedPayload exactly as
// internal/cmd/channelsd/interrupt_applied_bridge.go publishes it for a rejected
// interrupt: OutcomeText is couldntInterruptText(reason) and Reason is the
// same runner-supplied string, on the queued_messages category.
//
// Reproduced here rather than imported because the bridge lives in package
// main. Keep in step with interrupt_applied_bridge.go's handle(): if that
// producer stops feeding the runner's reason into these two fields, this
// fixture is what says so.
func appliedFromInterruptBridge(reason, responseURL string) channelevents.InteractionAppliedPayload {
	return channelevents.InteractionAppliedPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Category:        "queued_messages",
		RequestRef:      "r-interrupt",
		Outcome:         channelevents.OutcomeDenied,
		OutcomeText:     "Couldn't interrupt — " + reason,
		Reason:          reason,
		ResponseRef:     responseURL,
	}
}

// responseURLText captures the "text" field of the single response_url POST a
// resolved interaction pushes to the clicker's surface. Slack parses that
// field as mrkdwn, exactly like MsgOptionText(_, false).
func responseURLText(t *testing.T, s *interactionSender, p channelevents.InteractionAppliedPayload) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err, "read response_url body")
		var body struct {
			Text string `json:"text"`
		}
		require.NoError(t, json.Unmarshal(raw, &body), "decode response_url body")
		got = body.Text
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s.httpClient = responseURLClient(srv)
	p.ResponseRef = testResponseURL
	_, err := s.Send(context.Background(), sessionWithChannel("C_OUT", "1700000000.000001"),
		interactionEnvelope(t, channelevents.KindInteractionApplied, p))
	require.NoError(t, err, "Send")
	return got
}

// assertInert states the one contract every mrkdwn sink on this path owes: the
// runner's markup must not render, and must not be silently deleted either —
// a reader has to be able to see that something was attempted.
func assertInert(t *testing.T, got, surface string) {
	t.Helper()
	assert.NotContains(t, got, lureLink,
		surface+" is parsed as mrkdwn: an unescaped OutcomeText renders a forged platform action")
	assert.NotContains(t, got, "<!channel>",
		surface+" is parsed as mrkdwn: an unescaped OutcomeText opens a channel-wide ping")
	assert.Contains(t, got, lureLinkVisible,
		"...and the attempt must stay visible, not be deleted")
}

// TestDecisionApplied_OutcomeTextIsInertOnEveryNotificationSink pins the three
// notification-text sinks a resolved decision writes. All three carry the same
// interactionOutcomeText string, and all three are surfaces Slack parses:
// MsgOptionText(_, false) means slack-go does not escape the field for us, and
// the response_url body's "text" is the same kind of field over HTTP.
func TestDecisionApplied_OutcomeTextIsInertOnEveryNotificationSink(t *testing.T) {
	cases := []struct {
		name string
		// sentText resolves the text the sink actually put on the wire.
		sentText func(t *testing.T) string
	}{
		{
			name: "edited prompt: the notification preview carries no live markup",
			sentText: func(t *testing.T) string {
				t.Helper()
				fc := &fakeSlackClient{}
				s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}
				s.delivery.record("r-interrupt",
					deliveryRef{ChannelID: "DOWNER", TS: "1.1"}, deliveryRef{})
				_, err := s.Send(context.Background(), sessionWithChannel("C_OUT", "1700000000.000001"),
					interactionEnvelope(t, channelevents.KindInteractionApplied,
						appliedFromInterruptBridge(interruptReasonLure, "")))
				require.NoError(t, err, "Send")
				calls := fc.snapshotUpdateCalls()
				require.Len(t, calls, 1, "the recorded prompt ref is edited in place")
				return msgOptionText(t, calls[0].channelID, calls[0].opts)
			},
		},
		{
			name: "edited PUBLIC note: the notification preview carries no live markup",
			sentText: func(t *testing.T) string {
				t.Helper()
				fc := &fakeSlackClient{}
				s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}
				// Only a note ref: isolates the public-note edit as the one
				// UpdateMessageContext call, on the surface everyone sees.
				s.delivery.record("r-interrupt", deliveryRef{}, deliveryRef{ChannelID: "CHAN", TS: "2.2"})
				_, err := s.Send(context.Background(), sessionWithChannel("C_OUT", "1700000000.000001"),
					interactionEnvelope(t, channelevents.KindInteractionApplied,
						appliedFromInterruptBridge(interruptReasonLure, "")))
				require.NoError(t, err, "Send")
				calls := fc.snapshotUpdateCalls()
				require.Len(t, calls, 1, "the recorded public note is edited in place")
				return msgOptionText(t, calls[0].channelID, calls[0].opts)
			},
		},
		{
			name: "response_url copy: the clicker's own surface carries no live markup",
			sentText: func(t *testing.T) string {
				t.Helper()
				s := &interactionSender{client: &fakeSlackClient{}, delivery: newInteractionDeliveryStore()}
				return responseURLText(t, s, appliedFromInterruptBridge(interruptReasonLure, ""))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertInert(t, tc.sentText(t), "the resolved interaction's notification text")
		})
	}
}

// TestDecisionApplied_VerdictLineIsInertOnTheCard covers the sink the
// notification-text sweep does not reach: the resolved CARD's own body.
//
// escapePublisherPayload makes the cached REQUEST payload inert, but
// appliedVerdictLine composes OutcomeText and Reason in AFTER that sweep has
// run, and the composed body renders through buildInteractionBlocks as a
// MarkdownType section. Both fields are the runner's on this path, and this is
// the surface a reader is told a named human already decided.
func TestDecisionApplied_VerdictLineIsInertOnTheCard(t *testing.T) {
	p := appliedFromInterruptBridge(interruptReasonLure, "")
	got := interactionText(t, buildInteractionAppliedBlocks(p, nil))
	assertInert(t, got, "the resolved card's body section")
}

// TestDecisionApplied_ReasonIsInertOnTheCard isolates Reason from OutcomeText.
//
// The two are separate slots that happen to share a producer, and a fix that
// only escaped the label would leave this one live: the bridge copies the
// runner's reason across verbatim, and appliedVerdictLine appends it after the
// verdict as " — <reason>".
func TestDecisionApplied_ReasonIsInertOnTheCard(t *testing.T) {
	p := appliedFromInterruptBridge(interruptReasonLure, "")
	// A benign label proves the finding is Reason's own, not OutcomeText's.
	p.OutcomeText = "Couldn't interrupt"

	got := interactionText(t, buildInteractionAppliedBlocks(p, nil))
	assertInert(t, got, "the resolved card's verdict reason")
}

// TestDecisionApplied_DeciderMentionStaysLive is the false-positive guard on
// all of the above, and the reason appliedVerdictLine escapes and composes in
// ONE function: the decider mention is markup THIS kind resolved from a
// structured identity, so it must stay clickable. Escaping the composed line
// instead of its untrusted parts would render "&lt;@U_APPROVER&gt;" and the
// card would stop naming who decided.
func TestDecisionApplied_DeciderMentionStaysLive(t *testing.T) {
	p := appliedFromInterruptBridge(interruptReasonLure, "")
	p.DecidedBy = &channelevents.ExternalIdentity{
		Kind: identity.KindSlack, ExternalID: identity.RawExternalID("U_APPROVER"),
	}

	got := interactionText(t, buildInteractionAppliedBlocks(p, nil))
	assert.Contains(t, got, "<@U_APPROVER>",
		"the decider mention is markup this kind composed; the escape must run BEFORE it, not after")
}

// TestCredentialLinkResolved_LeadKeepsTheRawOutcomeText is the other
// false-positive guard: credential_link routes OutcomeText into the card's
// Lead, whose sink is noticeTitle's rich_text element — Slack renders that
// LITERALLY, so escaping there would show a reader "&amp;" for an ordinary
// ampersand in a credential name.
//
// That is the same per-SINK rule the Lead itself follows (inert.go), and it is
// why this one is deliberately NOT escaped. A future sweep that "fixes" it by
// symmetry should fail here.
func TestCredentialLinkResolved_LeadKeepsTheRawOutcomeText(t *testing.T) {
	const credential = "Acme R&D"
	blocks := buildInteractionResolvedBlocks(channelevents.InteractionAppliedPayload{
		Category:    "credential_link",
		RequestRef:  "cred-1",
		Outcome:     channelevents.OutcomeResolved,
		OutcomeText: credential,
	})

	require.NotEmpty(t, blocks, "the resolved credential notice renders a container")
	assert.Contains(t, concatBlockText(blocks), credential+" connected",
		"a rich_text title renders characters literally: escaping it would show the reader an entity")
}
