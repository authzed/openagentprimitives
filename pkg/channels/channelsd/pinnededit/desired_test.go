package pinnededit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pinnededit"
)

func anchored(phase string, pinned *v1alpha1.PinnedMessageStatus) *v1alpha1.AgentSession {
	return &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gh-sess", Namespace: "default",
			Annotations: map[string]string{v1alpha1.AnnotationSessionOpening: "Picked up PR #4"},
		},
		Spec: v1alpha1.AgentSessionSpec{OutputChannel: &v1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C_OUT", "thread_ts": "111.222"},
		}},
		Status: v1alpha1.AgentSessionStatus{Phase: phase, PinnedMessage: pinned},
	}
}

func TestDesired(t *testing.T) {
	// No output / no anchor -> not a candidate.
	_, ok := pinnededit.Desired(&v1alpha1.AgentSession{})
	assert.False(t, ok)

	// Running, in_progress projection.
	c, ok := pinnededit.Desired(anchored("Running", &v1alpha1.PinnedMessageStatus{Badge: v1alpha1.OpeningBadgeInProgress, Body: "wip"}))
	require.True(t, ok)
	assert.Equal(t, v1alpha1.OpeningBadgeInProgress, c.Badge)
	assert.Equal(t, "Picked up PR #4", c.OpeningText)
	assert.Equal(t, "C_OUT", c.Ref.ChannelID)
	assert.Equal(t, "111.222", c.Ref.TS)
	assert.Equal(t, "wip", c.Body)

	// Concluded outcome survives a later Failed phase (agent answered).
	c, _ = pinnededit.Desired(anchored("Failed", &v1alpha1.PinnedMessageStatus{Badge: v1alpha1.OpeningBadgeProblemsFound}))
	assert.Equal(t, v1alpha1.OpeningBadgeProblemsFound, c.Badge)

	// Failed with no outcome -> unfinished (even if pinnedMessage is nil).
	// Nil pinnedMessage must yield empty body/link, not a panic.
	c, _ = pinnededit.Desired(anchored("Failed", nil))
	assert.Equal(t, v1alpha1.OpeningBadgeUnfinished, c.Badge)
	assert.Equal(t, "", c.Body)
	assert.Equal(t, "", c.Link)

	// Succeeded with no outcome -> neutral done.
	c, _ = pinnededit.Desired(anchored("Succeeded", &v1alpha1.PinnedMessageStatus{Badge: v1alpha1.OpeningBadgeInProgress}))
	assert.Equal(t, v1alpha1.OpeningBadgeDone, c.Badge)
}

func TestSummaryUpdatesItsOwnMessage(t *testing.T) {
	sess := anchored(v1alpha1.AgentSessionPhaseRunning, nil)
	sess.Spec.OpeningSummary = "Session created to meet goal Stretch: Stand up."
	sess.Spec.Prompt.Inline = "  exact instructions\n"
	_, ok := pinnededit.Desired(sess)
	require.False(t, ok, "a summary without a posted-message reference must never edit the existing thread root")
	sess.Annotations[v1alpha1.AnnotationSessionOpeningMessageChannel] = "C_OUT"
	sess.Annotations[v1alpha1.AnnotationSessionOpeningMessageID] = "summary-message"
	content, ok := pinnededit.Desired(sess)
	require.True(t, ok)
	require.Equal(t, "summary-message", content.Ref.TS)
	require.Equal(t, sess.Spec.OpeningSummary, content.OpeningText)
	require.Equal(t, sess.Spec.Prompt.Inline, content.Instructions)
}

// TestDesiredGate covers the candidate gate's three independent rejection
// reasons: an OutputChannel present but missing exactly one of channel_id,
// thread_ts, or the opening-text annotation each yields ok=false.
func TestDesiredGate(t *testing.T) {
	build := func(external map[string]string, opening string) *v1alpha1.AgentSession {
		anns := map[string]string{}
		if opening != "" {
			anns[v1alpha1.AnnotationSessionOpening] = opening
		}
		return &v1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: "gh-sess", Namespace: "default", Annotations: anns,
			},
			Spec: v1alpha1.AgentSessionSpec{OutputChannel: &v1alpha1.ChannelBinding{
				External: external,
			}},
			Status: v1alpha1.AgentSessionStatus{Phase: "Running"},
		}
	}
	cases := []struct {
		name    string
		session *v1alpha1.AgentSession
	}{
		{
			name:    "OutputChannel set, no channel_id: ok=false",
			session: build(map[string]string{"thread_ts": "111.222"}, "Picked up PR #4"),
		},
		{
			name:    "OutputChannel set, no thread_ts: ok=false",
			session: build(map[string]string{"channel_id": "C_OUT"}, "Picked up PR #4"),
		},
		{
			name:    "OutputChannel + coords set, no opening annotation: ok=false",
			session: build(map[string]string{"channel_id": "C_OUT", "thread_ts": "111.222"}, ""),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := pinnededit.Desired(tc.session)
			assert.False(t, ok)
		})
	}
}
