package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// slackHumanInbound is a Slack-shaped inbound whose text opens with the BOT's
// mention — the exact shape that had an agent reply "Hey @<itself>" — and
// whose SenderMention names the human who sent it.
func slackHumanInbound(ch *spiceboxv1alpha1.Channel, senderMention string) channelkinds.InboundEvent {
	return channelkinds.InboundEvent{
		Channel:       ch,
		ExternalIDs:   channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:    "thread:C1:1",
		MessageText:   "<@U0BOT> can you check the build?",
		SenderMention: senderMention,
	}
}

// TestDeliverExistingSession_SenderMention covers the follow-up path: the
// sender's token rides on the turn as its own block, never folded into the
// text the person wrote.
func TestDeliverExistingSession_SenderMention(t *testing.T) {
	cases := []struct {
		name          string
		senderMention string
		wantContent   []MemContent
	}{
		{
			name:          "token supplied: text block untouched, sender block appended",
			senderMention: "<@U1>",
			wantContent: []MemContent{
				{Type: "text", Text: "<@U0BOT> can you check the build?"},
				{Type: memory.BlockTypeSenderMention, Text: "<@U1>"},
			},
		},
		{
			name:          "no token: text block only",
			senderMention: "",
			wantContent: []MemContent{
				{Type: "text", Text: "<@U0BOT> can you check the build?"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			existing := existingSession(t, "c1-abc", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
			p, _, mem, _, _ := newPipeline(t, ch, existing)

			dec, err := p.Deliver(context.Background(), slackHumanInbound(ch, tc.senderMention))
			require.NoError(t, err, "Deliver")
			require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
			require.Len(t, mem.appends, 1, "memory appends")
			assert.Equal(t, tc.wantContent, mem.appends[0].turn.Content)
		})
	}
}

// TestDeliverNewSession_StampsStarterMention covers the first message, which
// becomes spec.prompt and is placed as turn 0 by the runner — so its token has
// to ride on the Create rather than on a channelsd-written turn.
func TestDeliverNewSession_StampsStarterMention(t *testing.T) {
	cases := []struct {
		name          string
		senderMention string
		wantStamped   bool
	}{
		{name: "token supplied: annotation carries it", senderMention: "<@U1>", wantStamped: true},
		{name: "no token: no annotation", senderMention: "", wantStamped: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			p, _, _, _, _ := newPipeline(t, ch)

			dec, err := p.Deliver(context.Background(), slackHumanInbound(ch, tc.senderMention))
			require.NoError(t, err, "Deliver")
			require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

			sess := onlySession(t, p)
			got, ok := sess.Annotations[spiceboxv1alpha1.AnnotationStarterMention]
			assert.Equal(t, tc.wantStamped, ok, "annotation presence")
			assert.Equal(t, tc.senderMention, got)
		})
	}
}

// TestDecidePermissionApprove_ResubmittedTurnCarriesSenderMention covers the
// held-then-approved guest. Their message is replayed from the stashed
// requester, long after the listener's event (and the token it carried) is
// gone, so the token has to be derived again from the requester's own kind and
// external id — and this is the sender the agent is LEAST able to address
// without it, having never spoken to them before.
func TestDecidePermissionApprove_ResubmittedTurnCarriesSenderMention(t *testing.T) {
	cases := []struct {
		name        string
		requester   channelkinds.ExternalIdentity
		wantContent []MemContent
	}{
		{
			name:      "kind with a sender token: sender block appended after the stashed text",
			requester: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "UGUEST", Email: "guest@example.com"},
			wantContent: []MemContent{
				{Type: "text", Text: "<@U0BOT> can I join?"},
				{Type: memory.BlockTypeSenderMention, Text: "<@UGUEST>"},
			},
		},
		{
			name:      "kind with no mention syntax: text block only, never the bare id",
			requester: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "UGUEST", Email: "guest@example.com"},
			wantContent: []MemContent{
				{Type: "text", Text: "<@U0BOT> can I join?"},
			},
		},
		{
			name:      "unregistered kind: text block only",
			requester: channelkinds.ExternalIdentity{Kind: "no-such-kind", ExternalID: "UGUEST", Email: "guest@example.com"},
			wantContent: []MemContent{
				{Type: "text", Text: "<@U0BOT> can I join?"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := existingSession(t, "c1-held-guest", "U_OWNER", spiceboxv1alpha1.AgentSessionPhaseRunning)
			p, _, mem, _, _ := newPipeline(t, sess)
			seedPendingRequester(t, p, sess, "perm-held-1", tc.requester, "<@U0BOT> can I join?", "thread:C1:1")

			out, err := p.decidePermission(context.Background(), channelinteractions.Decision{
				Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
				Payload: channelevents.InteractionDecisionPayload{
					Category:   categories.PermissionRequest,
					RequestRef: "perm-held-1",
					ActionID:   "approve",
					Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER", Email: "owner@example.com"},
				},
			})
			require.NoError(t, err, "decidePermission")
			require.Equal(t, channelevents.OutcomeApproved, out.Result)
			require.Len(t, mem.appends, 1, "the approved message is replayed as one turn")
			assert.Equal(t, tc.wantContent, mem.appends[0].turn.Content)
		})
	}
}
