package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// A conversational subagent's tool approval, at the Slack boundary.
//
// The card is published by a child whose own outbound binding is an `agent`
// Channel and routed by the outbound relay to the ROOT's Slack. Two shapes of
// the same payload reach this sender depending on whether the relay's
// recipient re-stamp ran, and they behave completely differently here — which
// is what makes the re-stamp load-bearing rather than cosmetic.
//
// This is the Slack half of a contract whose other half is
// pkg/channels/channelsd/outbound/recipient_kind.go. The two meet on one
// value: the approver's Kind tag must be the DELIVERING channel's kind. The
// relay-side test asserts the re-stamp produces "slack"; these two assert what
// "slack" and "agent" each do once they arrive.

// conversationalChildApproval is the tool_approval a runner publishes for a
// delegated child: an approver resolved from agentsession#approve, carried as a
// canonical Subject (never a raw ExternalID — stuffing a canonical into
// ExternalID is the known silent-delivery bug), with the Kind tag the caller
// chooses.
func conversationalChildApproval(approverKind identity.Kind, email string) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		Category:   "tool_approval",
		RequestRef: "req-childapproval",
		Lead:       "Approve running git_push?",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope: channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{
				Kind:    approverKind,
				Subject: identity.Subject("user:" + emailCanonical(email)),
			}},
		},
	}
}

// TestInteractionSender_Request_ConversationalChildApprover_RestampedKindIsActuallyAddressed
// is the delivered half. Given the payload as the outbound relay hands it over
// — the approver's Kind re-stamped onto the channel the card was routed to —
// the approver is resolved and the prompt lands on them. The assertion is on
// the RECIPIENT being addressed (the ephemeral's userID), not on a card having
// been built: a card nobody is asked about is the exact failure this covers.
func TestInteractionSender_Request_ConversationalChildApprover_RestampedKindIsActuallyAddressed(t *testing.T) {
	const (
		email    = "dana@corp.example"
		slackID  = "U_DANA"
		chanID   = "C_ROOT_THREAD"
		threadTS = "1700000000.000020"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c, delivery: newInteractionDeliveryStore()}

	p := conversationalChildApproval("slack", email)
	_, err := s.Send(context.Background(), sessionWithChannel(chanID, threadTS),
		interactionEnvelope(t, channelevents.KindInteractionRequest, p))
	require.NoError(t, err, "Send must succeed")

	require.Len(t, c.postEphemeralCalls, 1, "the approver must actually be asked")
	assert.Equal(t, slackID, c.postEphemeralCalls[0].userID,
		"the approver is addressed by the resolved canonical, on the channel the card was routed to")
	assert.Empty(t, c.postMessageCalls, "nothing undeliverable was reported: someone was asked")
}

// TestInteractionSender_Request_ConversationalChildApprover_AgentKindIsSkippedAndUndeliverable
// is the negative control, and it is the bug. The SAME approver, stamped with
// the child's own `agent` binding kind — which is what every publisher produces
// without the relay's re-stamp, because the runner's Role cannot read an
// ancestor to learn any better — is skipped by the recipient gate. Nobody is
// asked, and the session parks awaiting a decision that was never requested.
//
// It asserts the skip AND the surfacing: loud-and-useless is still useless, and
// keeping both halves here is what stops a future change from "fixing" the
// noise by removing the notice instead of the cause.
func TestInteractionSender_Request_ConversationalChildApprover_AgentKindIsSkippedAndUndeliverable(t *testing.T) {
	const (
		email    = "dana@corp.example"
		slackID  = "U_DANA"
		chanID   = "C_ROOT_THREAD"
		threadTS = "1700000000.000021"
	)
	c := fakeClientWithUser(email, slackID)
	s := &interactionSender{client: c, delivery: newInteractionDeliveryStore()}

	p := conversationalChildApproval("agent", email)
	_, err := s.Send(context.Background(), sessionWithChannel(chanID, threadTS),
		interactionEnvelope(t, channelevents.KindInteractionRequest, p))
	require.NoError(t, err, "an undeliverable fan-out is surfaced, not returned as an error")

	assert.Empty(t, c.postEphemeralCalls,
		"an approver tagged with a kind this surface does not serve is skipped, however resolvable they are")
	assert.Empty(t, c.lookupByEmailCalls,
		"the gate fires before any resolution is attempted, which is why the identity being valid does not save it")
	require.Len(t, c.postMessageCalls, 1, "the thread must be told nothing is waiting on it")
	assert.Contains(t, strings.ToLower(msgOptionText(t, c.postMessageCalls[0].channelID, c.postMessageCalls[0].options)),
		"couldn't reach anyone")
}
