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

// queuedFixture builds a Channel of the given kind plus an active session on
// the "thread:C1:1" key in the given phase — the two objects every row of
// TestDeliverActiveSession_ReportsQueuedBehindLiveTurn varies.
func queuedFixture(t *testing.T, kind, phase string) (*spiceboxv1alpha1.Channel, *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: kind, AgentClass: "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c1-abc", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c1",
				spiceboxv1alpha1.LabelChannelKey:  sha256HexTest("thread:C1:1"),
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: kind, Key: "thread:C1:1",
				NATSSubjectPrefix: "ap.session.default.c1-abc",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
	return ch, sess
}

// TestDeliverActiveSession_ReportsQueuedBehindLiveTurn pins the signal the
// channel kinds need to render a mid-turn inbound honestly.
//
// Deliver publishes the "your message is queued" ack when the session is
// Running, but a bare OutcomeRouted is indistinguishable from an inbound that
// actually starts a turn: the Slack listener then announces "<agent> is
// starting…" for a queued message, contradicting the ack it just triggered and
// clobbering the live turn's progress caption.
//
// Queued is kind-independent — it describes the session's state (a turn is in
// flight), not how a transport renders it — so it is set for the enqueue-ack
// kinds too, not only inside the Slack interaction_request branch.
func TestDeliverActiveSession_ReportsQueuedBehindLiveTurn(t *testing.T) {
	cases := []struct {
		name       string
		kind       string
		phase      string
		wantQueued bool
	}{
		{
			name:       "Running slack session: routed AND queued behind the live turn",
			kind:       "slack",
			phase:      spiceboxv1alpha1.AgentSessionPhaseRunning,
			wantQueued: true,
		},
		{
			name:       "Running non-slack session: routed AND queued (legacy enqueue-ack kinds too)",
			kind:       "fake",
			phase:      spiceboxv1alpha1.AgentSessionPhaseRunning,
			wantQueued: true,
		},
		{
			name:       "Idle session: routed and NOT queued — this inbound starts the turn",
			kind:       "fake",
			phase:      spiceboxv1alpha1.AgentSessionPhaseIdle,
			wantQueued: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, sess := queuedFixture(t, tc.kind, tc.phase)
			p, _, _, _, _ := newPipeline(t, ch, sess)

			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:     ch,
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
				ChannelKey:  "thread:C1:1",
				MessageText: "are you still there?",
			})
			require.NoError(t, err, "Deliver")
			assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
			assert.Equal(t, tc.wantQueued, dec.Queued,
				"InboundDecision.Queued for a %s-phase session", tc.phase)
		})
	}
}
