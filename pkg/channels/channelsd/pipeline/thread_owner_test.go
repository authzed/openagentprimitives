package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// channelForAgent builds a Channel CR bound to a named AgentClass.
func channelForAgent(name, agentClass string) *spiceboxv1alpha1.Channel {
	ch := newChannel(name)
	ch.Spec.AgentClass = agentClass
	return ch
}

// sessionOwningThread builds a cron-shaped AgentSession that owns a thread:
// its slack anchor lives on spec.outputChannel and is indexed by
// LabelOutputChannelKey, the label channelsd's outbound relay stamps after the
// first send.
func sessionOwningThread(name, agentClass, threadKey string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelOutputChannelKey: channelkey.LabelValue(threadKey),
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: agentClass,
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "cron-output", Kind: "slack", Key: threadKey,
				External: map[string]string{"channel_id": "C1", "thread_ts": "9.9"},
			},
		},
	}
}

// A thread is owned 1:1 by the agent whose session is bound to it. A different
// agent must neither route into that session nor create a second session of
// its own on the same thread.
//
// Deliver's output-key fallback exists so a human reply in a cron thread finds
// the cron-spawned session (whose inbound anchor is per-firing and never
// repeats). That lookup is namespace-wide and carries no agent filter, so when
// several Slack apps are members of one conversation, every one of their
// listeners resolved the SAME cron session and delivered the same human
// message into it once per app.
func TestDeliver_ThreadOwnedByAnotherAgentIsNeitherRoutedNorRebound(t *testing.T) {
	const threadKey = "thread:C1:9.9"

	owner := sessionOwningThread("hubspot-cron-1", "hubspot-companies", threadKey)
	intruder := channelForAgent("demo-channel", "demo-agent")

	p, _, _, _, cli := newPipeline(t, intruder, owner)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     intruder,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_A", Email: "a@example.com"},
		ChannelKey:  threadKey,
		MessageText: "a human reply in the other agent's thread",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "9.9", "message_ts": "11.0"},
	})
	require.NoError(t, err)

	assert.NotEqual(t, channelkinds.OutcomeRouted, dec.Outcome,
		"a different agent must not route into the session that owns this thread")
	assert.NotEqual(t, "hubspot-cron-1", dec.Session.Name,
		"the owning agent's session must not be resolved for a foreign Channel")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 1,
		"no second session may be bound to a thread another agent already owns")
}

// Adoption is the deliberate "brought in" case: a human @-mentions a second
// agent into an existing thread. That agent gets its own session with its own
// binding (mention_only), so the 1:1 rule is not violated — the guard must not
// block it.
func TestDeliver_AdoptionMayStillJoinAnotherAgentsThread(t *testing.T) {
	const threadKey = "thread:C1:9.9"

	owner := sessionOwningThread("hubspot-cron-1", "hubspot-companies", threadKey)
	joiner := channelForAgent("slack-demo", "codebot")

	p, _, _, _, cli := newPipeline(t, joiner, owner)
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return channelkinds.HistoryPage{
			Messages: []channelkinds.HistoryMessage{
				{AuthorExternalID: "U_A", AuthorDisplayName: "Alice", AuthorEmail: "a@example.com", Text: "older", TS: "10.1"},
			},
		}, nil
	}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     joiner,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_S", Email: "s@example.com"},
		ChannelKey:  threadKey,
		MessageText: "@codebot take a look",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "9.9", "message_ts": "11.0"},
	})
	require.NoError(t, err)
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome,
		"an explicitly summoned agent adopts the thread with its own session")
	assert.True(t, dec.Adopted)

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 2,
		"the summoned agent gets its own session alongside the thread's owner")
}

// A DM channel key is scoped to ONE Channel — "dm:<user>" means "my DM with
// this user", not a globally shared surface. Two bots DM'd by the same person
// are separate conversations that legitimately share a key string, and Slack
// gives each bot its own DM channel_id.
//
// So the guard must key off the shared external anchor (channel_id +
// thread_ts), not the opaque key hash. Keying off the hash would mean the
// first person to DM a second bot is refused, because the first bot's session
// carries the identical key — silently breaking DMs for every bot after the
// first.
func TestDeliver_DMToASecondAgentIsNotBlockedByTheFirst(t *testing.T) {
	const dmKey = "dm:U1"

	first := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "first-bot-dm", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "first-bot-channel",
				spiceboxv1alpha1.LabelChannelKey:  channelkey.LabelValue(dmKey),
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "first-agent",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "first-bot-channel", Kind: "slack", Key: dmKey,
				External: map[string]string{"channel_id": "D_FIRST", "message_ts": "10.0"},
			},
		},
	}
	second := channelForAgent("second-bot-channel", "second-agent")

	p, _, _, _, cli := newPipeline(t, second, first)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     second,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "u1@example.com"},
		ChannelKey:  dmKey,
		MessageText: "hello second bot",
		External:    map[string]string{"channel_id": "D_SECOND", "message_ts": "11.0"},
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome,
		"a DM to a second bot is its own conversation, not a claim on the first bot's")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 2, "the second bot gets its own DM session")
}

// The fallback must keep doing its job for the agent that DOES own the thread:
// a reply routes into the cron-spawned session even though that session's
// inbound anchor never matches the thread key.
func TestDeliver_OwningAgentStillResolvesItsCronThread(t *testing.T) {
	const threadKey = "thread:C1:9.9"

	owner := sessionOwningThread("hubspot-cron-1", "hubspot-companies", threadKey)
	owner.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	ch := channelForAgent("slack-hubspot-companies", "hubspot-companies")

	p, _, _, _, _ := newPipeline(t, ch, owner)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_A", Email: "a@example.com"},
		ChannelKey:  threadKey,
		MessageText: "a reply from the owning agent's own Slack app",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "9.9", "message_ts": "11.0"},
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Equal(t, "hubspot-cron-1", dec.Session.Name,
		"the owning agent's reply must still resolve its cron session via the output-key fallback")
}
