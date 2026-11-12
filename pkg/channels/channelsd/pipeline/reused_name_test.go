package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// A session name is derived from (channel, key), and a session's memory scope
// is keyed by that name — not by the CR's UID — and outlives the CR by design
// (the transcript is append-only audit evidence). So when a session is deleted
// and the same person messages the same channel key again, the deterministic
// name lands the fresh session on its predecessor's transcript: the runner
// replays it as a resume and never places the new message, with the
// predecessor's tool output as working context.
//
// These cases pin the fix: a fresh session is never minted over a scope that
// already holds turns.
func TestFreshMint_ReusedNameWithRetainedTranscript(t *testing.T) {
	const key = "dm:U1"
	ctx := context.Background()
	predecessorTurns := []MemTurn{
		{Index: 0, Role: "user", Content: []MemContent{{Type: "text", Text: "what is in the shared drive?"}}},
		{Index: 1, Role: "assistant", Content: []MemContent{{Type: "text", Text: "the payroll sheet"}}},
	}

	cases := []struct {
		name string
		// seededGenerations names, by generation, the scopes that already hold
		// a dead predecessor's turns: 1 is makeSessionName's own name, 2 its
		// first successor, and so on.
		seededGenerations []int
		// liveAt, when non-zero, pre-creates a session CR (carrying no channel
		// labels, so Deliver's correlation misses it — exactly the state a
		// concurrent replica's just-minted session is in) at that generation.
		liveAt int
		// terminating marks the liveAt session as already being deleted.
		terminating bool
		readErr     error
		wantErr     bool
		wantOut     channelkinds.Outcome
		// wantGeneration is the generation the inbound's session ends up under;
		// 0 means no session may be created at all.
		wantGeneration int
	}{
		{
			name:           "empty scope: the deterministic name is used unchanged",
			wantOut:        channelkinds.OutcomeRouted,
			wantGeneration: 1,
		},
		{
			name:              "scope holds a deleted predecessor's transcript: minted under the next generation",
			seededGenerations: []int{1},
			wantOut:           channelkinds.OutcomeRouted,
			wantGeneration:    2,
		},
		{
			name:              "two predecessors deleted in turn: walks past both to the first empty scope",
			seededGenerations: []int{1, 2},
			wantOut:           channelkinds.OutcomeRouted,
			wantGeneration:    3,
		},
		{
			name:              "a live session holds the occupied scope (replica race): joins it, mints no second session",
			seededGenerations: []int{1},
			liveAt:            1,
			wantOut:           channelkinds.OutcomeRouted,
			wantGeneration:    1,
		},
		{
			// Its finalizer is running: it is the predecessor, not a concurrent
			// mint, and joining it would route the inbound into a session that
			// is about to vanish.
			name:              "the session holding the occupied scope is being deleted: minted under the next generation",
			seededGenerations: []int{1},
			liveAt:            1,
			terminating:       true,
			wantOut:           channelkinds.OutcomeRouted,
			wantGeneration:    2,
		},
		{
			name:    "memory unreadable: fail closed, InternalError and no session minted",
			readErr: errors.New("memory backend unavailable"),
			wantErr: true,
			wantOut: channelkinds.OutcomeInternalError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			objs := []client.Object{ch}
			if tc.liveAt != 0 {
				existing := &spiceboxv1alpha1.AgentSession{
					ObjectMeta: metav1.ObjectMeta{Name: generationName(ch, key, tc.liveAt), Namespace: ch.Namespace},
					Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
				}
				if tc.terminating {
					now := metav1.Now()
					existing.DeletionTimestamp = &now
					existing.Finalizers = []string{spiceboxv1alpha1.FinalizerAgentSession}
				}
				objs = append(objs, existing)
			}
			p, _, mem, _, cli := newPipeline(t, objs...)
			for _, g := range tc.seededGenerations {
				mem.seeded[ch.Namespace+"/"+generationName(ch, key, g)] = predecessorTurns
			}
			mem.readErr = tc.readErr

			dec, err := p.Deliver(ctx, channelkinds.InboundEvent{
				Channel:     ch,
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
				ChannelKey:  key,
				MessageText: "hello again",
			})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantOut, dec.Outcome)

			var sessions spiceboxv1alpha1.AgentSessionList
			require.NoError(t, cli.List(ctx, &sessions))
			live := slices.DeleteFunc(sessions.Items, func(s spiceboxv1alpha1.AgentSession) bool {
				return s.DeletionTimestamp != nil
			})
			if tc.wantGeneration == 0 {
				assert.Empty(t, live, "no session may be minted over a scope whose contents could not be read")
				return
			}
			require.Len(t, live, 1, "exactly one live session for one inbound")
			got := live[0]
			assert.Equal(t, generationName(ch, key, tc.wantGeneration), got.Name)
			if tc.liveAt == tc.wantGeneration {
				return // the pre-existing session is not this inbound's to shape
			}
			assert.Equal(t, "hello again", got.Spec.Prompt.Inline,
				"the inbound must be the new session's opening prompt")
			assert.Equal(t, channelkey.LabelValue(key), got.Labels[spiceboxv1alpha1.LabelChannelKey],
				"the correlation labels are what route the next message in this conversation here")
			require.NotNil(t, got.Spec.InputChannel)
			assert.Equal(t, key, got.Spec.InputChannel.Key)
			assert.Contains(t, got.Spec.InputChannel.NATSSubjectPrefix, got.Name,
				"the subject prefix must follow the name actually minted, not the occupied one")
		})
	}
}

// The follow-up half: a session minted under a successor name must still be
// the one the NEXT message in the same conversation reaches. Routing is by the
// correlation labels, never by recomputing the name, so this pins that the
// successor carries them and nothing on the routed path re-derives the name.
func TestFreshMint_SuccessorNameReceivesTheFollowUp(t *testing.T) {
	const key = "dm:U1"
	ctx := context.Background()
	ch := newChannel("c1")
	p, _, mem, _, cli := newPipeline(t, ch)
	mem.seeded[ch.Namespace+"/"+makeSessionName(ch, key)] = []MemTurn{
		{Index: 0, Role: "user", Content: []MemContent{{Type: "text", Text: "old question"}}},
		{Index: 1, Role: "assistant", Content: []MemContent{{Type: "text", Text: "old answer"}}},
	}
	ev := func(text string) channelkinds.InboundEvent {
		return channelkinds.InboundEvent{
			Channel:     ch,
			ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
			ChannelKey:  key,
			MessageText: text,
		}
	}

	dec, err := p.Deliver(ctx, ev("hello again"))
	require.NoError(t, err, "first Deliver mints the successor")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	successor := generationName(ch, key, 2)

	dec, err = p.Deliver(ctx, ev("and a follow-up"))
	require.NoError(t, err, "second Deliver routes into the successor")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Equal(t, successor, dec.Session.Name, "the follow-up must reach the successor session")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(ctx, &sessions))
	assert.Len(t, sessions.Items, 1, "a follow-up must not mint another session")
	require.Len(t, mem.appends, 1, "the follow-up is the only turn channelsd writes")
	assert.Equal(t, successor, mem.appends[0].name, "into the successor's scope, never the predecessor's")
	require.NotEmpty(t, mem.appends[0].turn.Content)
	assert.Equal(t, "and a follow-up", mem.appends[0].turn.Content[0].Text)
}

// The longest name the walk can mint must still leave room for the objects
// named after a session. The tightest is the workspace Job, "ws-apply-<name>",
// whose name must fit a 63-byte label value.
func TestSessionNameGeneration_LongestNameFitsAJobName(t *testing.T) {
	ch := newChannel(strings.Repeat("c", 80)) // truncated to 40 by makeSessionName
	name := sessionNameGeneration(makeSessionName(ch, "dm:U1"), maxSessionNameGenerations)
	assert.LessOrEqual(t, len("ws-apply-"+name), 63, "longest successor name %q", name)
}

// generationName is the name the g-th session for (ch, key) is minted under:
// makeSessionName's own for the first, then the successors the mint path
// walks to when a predecessor's transcript still occupies the scope.
func generationName(ch *spiceboxv1alpha1.Channel, key string, g int) string {
	return sessionNameGeneration(makeSessionName(ch, key), g)
}
