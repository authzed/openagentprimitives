package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// catchUpSession returns an adopted mention_only session whose backfill cursor
// has already advanced past the thread root.
func catchUpSession(t *testing.T, name, through string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationBackfilledThroughTS: through,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: "fake", Key: "thread:C1:1",
				RoutingMode: "mention_only",
			},
		},
	}
}

// TestCatchUp_SeedsThirdPartyAppMessagesButNotTheAgentsOwn is the guard for
// alerts that fire WHILE the agent is already in the thread. The blanket
// FromApp skip dropped them entirely — for an SRE bot those alerts are the
// highest-value content in the channel. The agent's own replies must still be
// excluded: they are already in its memory, and re-seeding them would double
// every turn it has taken.
func TestCatchUp_SeedsThirdPartyAppMessagesButNotTheAgentsOwn(t *testing.T) {
	sess := catchUpSession(t, "s1", "100.0")
	p, _, mem, _, _ := newPipeline(t, sess)
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string,
		_ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return channelkinds.HistoryPage{Messages: []channelkinds.HistoryMessage{
			{AuthorDisplayName: "srebot", Text: "on it, checking the pods", TS: "100.5", FromApp: true, FromSelf: true},
			{AuthorDisplayName: "alertbot", Text: "[FIRING:1] cache tier is down", TS: "100.6", FromApp: true},
			{AuthorExternalID: "U_ALICE", AuthorDisplayName: "Alice", Text: "any update?", TS: "100.7"},
		}}, nil
	}

	p.catchUp(context.Background(), sess, channelkinds.InboundEvent{
		Channel:    newChannel("c1"),
		ChannelKey: "thread:C1:1",
		External:   map[string]string{"message_ts": "200.0"},
	})

	require.Len(t, mem.appends, 1, "one catch-up transcript turn")
	got := mem.appends[0].turn.Content[0].Text

	assert.Contains(t, got, "[FIRING:1] cache tier is down",
		"a third-party alert posted mid-thread is content the agent has never seen")
	assert.Contains(t, got, "alertbot:", "and it must be attributed")
	assert.Contains(t, got, "any update?", "human messages still seed")
	assert.NotContains(t, got, "on it, checking the pods",
		"the agent's own reply is already in memory and must not be re-seeded")
}

// TestCatchUp_NoTurnWhenOnlyTheAgentsOwnRepliesAreNew keeps the empty-delta
// path honest: if nothing but the agent's own messages happened, there is
// nothing to tell it, and appending an empty transcript would burn a turn.
func TestCatchUp_NoTurnWhenOnlyTheAgentsOwnRepliesAreNew(t *testing.T) {
	sess := catchUpSession(t, "s2", "100.0")
	p, _, mem, _, _ := newPipeline(t, sess)
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string,
		_ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return channelkinds.HistoryPage{Messages: []channelkinds.HistoryMessage{
			{AuthorDisplayName: "srebot", Text: "on it", TS: "100.5", FromApp: true, FromSelf: true},
		}}, nil
	}

	p.catchUp(context.Background(), sess, channelkinds.InboundEvent{
		Channel:    newChannel("c1"),
		ChannelKey: "thread:C1:1",
		External:   map[string]string{"message_ts": "200.0"},
	})

	assert.Empty(t, mem.appends, "no delta worth telling the agent about")
}
