package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// attendedChildSession is a Running, channel-bound session (same shape
// existingSession builds) additionally carrying Task 5's watch relationship:
// LabelAttendedParentNamespace/Name naming the session that watches it.
func attendedChildSession(t *testing.T, name, parentNS, parentName string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	sess := existingSession(t, name, "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	sess.Labels[spiceboxv1alpha1.LabelAttendedParentNamespace] = parentNS
	sess.Labels[spiceboxv1alpha1.LabelAttendedParentName] = parentName
	return sess
}

// watchingParentSession is an Idle (WakeEligible) session carrying the given
// AgentWakeCredit — the human-anchored budget the mention-triggered wake
// spends against, unchanged by anything under test here.
func watchingParentSession(t *testing.T, name string, credit *int) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:           spiceboxv1alpha1.AgentSessionPhaseIdle,
			AgentWakeCredit: credit,
		},
	}
}

// parentAppended reports whether mem recorded an append to (ns, name).
func parentAppended(mem *fakeMemory, ns, name string) bool {
	for _, a := range mem.appends {
		if a.ns == ns && a.name == name {
			return true
		}
	}
	return false
}

// deliverToAttendedChild sends one U1 inbound to child on channel c1's
// default thread ("thread:C1:1", the key existingSession/attendedChildSession
// bind to) and returns the fresh pipeline state.
func deliverToAttendedChild(t *testing.T, ch *spiceboxv1alpha1.Channel, parent, child *spiceboxv1alpha1.AgentSession, text string) (channelkinds.InboundDecision, *fakeMemory, *fakeNATS, client.Client) {
	t.Helper()
	p, _, mem, nats, cli := newPipeline(t, ch, parent, child)
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: text,
	})
	require.NoError(t, err, "Deliver(%q)", text)
	return dec, mem, nats, cli
}

// TestAttendedChildTurn_NoMention_ParentSeesButIsNotWoken is the "see, don't
// react" half of the split: the parent's own memory picks up the child's
// ordinary turn even though nothing wakes it. Credit is left non-zero so the
// absence of a wake is attributable to the missing mention, not a spent
// budget — see the exhausted-credit case below for that half.
func TestAttendedChildTurn_NoMention_ParentSeesButIsNotWoken(t *testing.T) {
	ch := newChannel("c1")
	two := 2
	parent := watchingParentSession(t, "builder-parent", &two)
	child := attendedChildSession(t, "test-child", parent.Namespace, parent.Name)

	dec, mem, nats, cli := deliverToAttendedChild(t, ch, parent, child, "just chatting about the new tool")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")

	assert.True(t, parentAppended(mem, parent.Namespace, parent.Name),
		"the parent must see every child turn, mention or not")

	var gotParent spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: parent.Namespace, Name: parent.Name}, &gotParent))
	assert.Empty(t, gotParent.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"an ordinary, non-mentioning child turn must not respawn the parent")

	prefix := channelevents.SubjectPrefix(parent.Namespace, parent.Name)
	for _, s := range nats.subjects {
		assert.False(t, strings.HasPrefix(s, prefix), "no NATS wakeup for the parent on a non-mentioning turn: got %q", s)
	}
}

// TestAttendedChildTurn_MentionsParent_ParentSeesAndWakes is the "react" half:
// an explicit @-mention of the parent both appends AND wakes it, through the
// same applyWake (annotation + NATS) the child's own turn uses.
func TestAttendedChildTurn_MentionsParent_ParentSeesAndWakes(t *testing.T) {
	ch := newChannel("c1")
	two := 2
	parent := watchingParentSession(t, "builder-parent", &two)
	child := attendedChildSession(t, "test-child", parent.Namespace, parent.Name)

	dec, mem, nats, cli := deliverToAttendedChild(t, ch, parent, child, "hey @builder-parent can you take a look at this")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")

	assert.True(t, parentAppended(mem, parent.Namespace, parent.Name),
		"the parent must see the mentioning turn too")

	var gotParent spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: parent.Namespace, Name: parent.Name}, &gotParent))
	assert.NotEmpty(t, gotParent.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"an @-mention of the parent must respawn it (Idle is WakeEligible)")

	prefix := channelevents.SubjectPrefix(parent.Namespace, parent.Name)
	var sawParentWakeup bool
	for _, s := range nats.subjects {
		if strings.HasPrefix(s, prefix) {
			sawParentWakeup = true
		}
	}
	assert.True(t, sawParentWakeup, "an @-mention of the parent must also publish its NATS wakeup")
}

// TestAttendedChildTurn_MentionsParent_CreditExhausted_StillNotWoken pins the
// reused WakeCredit gate: a mention is the trigger, not a bypass. Without
// credit as the structural bound, a manipulated child could force unbounded
// parent wakes just by repeating the mention.
func TestAttendedChildTurn_MentionsParent_CreditExhausted_StillNotWoken(t *testing.T) {
	ch := newChannel("c1")
	zero := 0
	parent := watchingParentSession(t, "builder-parent", &zero)
	child := attendedChildSession(t, "test-child", parent.Namespace, parent.Name)

	dec, mem, nats, cli := deliverToAttendedChild(t, ch, parent, child, "hey @builder-parent can you take a look")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")

	assert.True(t, parentAppended(mem, parent.Namespace, parent.Name),
		"seeing is free even when the wake credit is exhausted")

	var gotParent spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: parent.Namespace, Name: parent.Name}, &gotParent))
	assert.Empty(t, gotParent.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"a mention cannot force a wake past an exhausted budget")

	prefix := channelevents.SubjectPrefix(parent.Namespace, parent.Name)
	for _, s := range nats.subjects {
		assert.False(t, strings.HasPrefix(s, prefix), "no wakeup for the parent once its credit is spent: got %q", s)
	}
}

// TestAttendedChildTurn_ParentMissing_DoesNotFailTheChildsOwnInbound covers
// the best-effort contract: a watch relationship naming a parent that no
// longer resolves (deleted, typo'd label, a race with GC) must not turn an
// otherwise-successful child inbound into a failure. The child still gets
// its own turn.
func TestAttendedChildTurn_ParentMissing_DoesNotFailTheChildsOwnInbound(t *testing.T) {
	ch := newChannel("c1")
	// The child names a parent that was never seeded — resolvable neither by
	// the mirror nor by anything else in this test.
	child := attendedChildSession(t, "test-child", "default", "no-such-parent")
	p, _, mem, _, _ := newPipeline(t, ch, child)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "an unresolvable watch relationship must not fail the child's own inbound")

	require.Len(t, mem.appends, 1, "only the child's own turn is recorded; there is no parent to mirror to")
	assert.Equal(t, "test-child", mem.appends[0].name)
}

// TestResubmitAuthorized_AttendedChild_MirrorsToParent covers the SECOND
// place this pipeline appends a turn to an active session's own memory:
// ResubmitAuthorized, the replay fired when a not-yet-authorized sender's
// message was stashed by handlePermissionDeny and later approved
// (decidePermission's auto-resubmit). A denied-then-approved sender's first
// turn on an attended child is exactly this path, and the parent's "sees
// every turn" guarantee must not carve out an exception for a turn that
// happened to need an extra approval round trip — same append, same
// wake-credit gate as the primary Deliver seam.
func TestResubmitAuthorized_AttendedChild_MirrorsToParent(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantWoken bool
	}{
		{name: "no mention: parent sees the resubmitted turn but is not woken", text: "here's the follow-up you asked for", wantWoken: false},
		{name: "mentions the parent: parent sees the resubmitted turn and wakes", text: "hey @builder-parent, take a look at this", wantWoken: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			two := 2
			parent := watchingParentSession(t, "builder-parent", &two)
			child := attendedChildSession(t, "test-child", parent.Namespace, parent.Name)
			p, _, mem, nats, cli := newPipeline(t, ch, parent, child)

			err := p.ResubmitAuthorized(context.Background(), child, channelkinds.InboundEvent{
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
				ChannelKey:  "thread:C1:1",
				MessageText: tc.text,
			})
			require.NoError(t, err, "ResubmitAuthorized")

			assert.True(t, parentAppended(mem, parent.Namespace, parent.Name),
				"the parent must see a resubmitted/approved turn too, not just the ones delivered on the first try")

			var gotParent spiceboxv1alpha1.AgentSession
			require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: parent.Namespace, Name: parent.Name}, &gotParent))
			prefix := channelevents.SubjectPrefix(parent.Namespace, parent.Name)
			var sawParentWakeup bool
			for _, s := range nats.subjects {
				if strings.HasPrefix(s, prefix) {
					sawParentWakeup = true
				}
			}
			if tc.wantWoken {
				assert.NotEmpty(t, gotParent.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
					"a mention on the resubmit path must respawn the parent (Idle is WakeEligible)")
				assert.True(t, sawParentWakeup, "a mention on the resubmit path must also publish the parent's NATS wakeup")
			} else {
				assert.Empty(t, gotParent.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
					"a non-mentioning resubmit must not respawn the parent")
				assert.False(t, sawParentWakeup, "a non-mentioning resubmit must not publish the parent's NATS wakeup")
			}
		})
	}
}

// TestMentionsAttendedParent pins the pure text-matching rule in isolation
// from the pipeline wiring above.
func TestMentionsAttendedParent(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		parentName string
		want       bool
	}{
		{name: "text names the parent: mentioned", text: "hey @builder-parent look at this", parentName: "builder-parent", want: true},
		{name: "text says nothing about the parent: not mentioned", text: "just chatting", parentName: "builder-parent", want: false},
		{name: "empty parent name: never mentioned", text: "@builder-parent", parentName: "", want: false},
		{name: "empty text: never mentioned", text: "", parentName: "builder-parent", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mentionsAttendedParent(tc.text, tc.parentName))
		})
	}
}

// TestAttendedParentOf pins the watch-relationship resolver: BOTH labels
// must be present, or the session reads as not watched at all — a partial
// pair (one label present without the other) is not a resolvable parent.
func TestAttendedParentOf(t *testing.T) {
	cases := []struct {
		name       string
		labels     map[string]string
		wantOK     bool
		wantNS     string
		wantParent string
	}{
		{
			name: "both labels present: resolves the parent",
			labels: map[string]string{
				spiceboxv1alpha1.LabelAttendedParentNamespace: "default",
				spiceboxv1alpha1.LabelAttendedParentName:      "builder-parent",
			},
			wantOK: true, wantNS: "default", wantParent: "builder-parent",
		},
		{name: "no labels at all: not an attended child", labels: nil, wantOK: false},
		{
			name:   "namespace label only: not resolvable",
			labels: map[string]string{spiceboxv1alpha1.LabelAttendedParentNamespace: "default"},
			wantOK: false,
		},
		{
			name:   "name label only: not resolvable",
			labels: map[string]string{spiceboxv1alpha1.LabelAttendedParentName: "builder-parent"},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Labels: tc.labels}}
			ns, name, ok := attendedParentOf(sess)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantNS, ns)
				assert.Equal(t, tc.wantParent, name)
			}
		})
	}
}
