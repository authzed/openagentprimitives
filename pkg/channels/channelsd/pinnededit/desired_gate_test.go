package pinnededit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pinnededit"

	// Register the channel kinds + the idle-status gate under test.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelsd/pinnededit/gates/triggeredidle"
)

func triggeredSess(phase, inputKind, startedBy string, pinned *v1alpha1.PinnedMessageStatus) *v1alpha1.AgentSession {
	ann := map[string]string{v1alpha1.AnnotationSessionOpening: "Picked up PR #4"}
	if startedBy != "" {
		ann[v1alpha1.AnnotationStartedByCanonicalID] = startedBy
	}
	return &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-sess", Namespace: "default", Annotations: ann},
		Spec: v1alpha1.AgentSessionSpec{
			InputChannel:  &v1alpha1.ChannelBinding{Name: "in", Kind: inputKind},
			OutputChannel: &v1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C_OUT", "thread_ts": "111.222"}},
		},
		Status: v1alpha1.AgentSessionStatus{Phase: phase, PinnedMessage: pinned},
	}
}

// A triggered (github check-run), humanless session that parks Idle must NOT
// keep reading "in progress" — the idle-status gate forces a terminal badge —
// while the session's PHASE stays Idle (so a later push/redelivery re-joins it).
// A human-started or conversational Idle session is untouched, a Running session
// is untouched, and a recorded outcome still wins.
func TestDesired_IdleStatusGate(t *testing.T) {
	cases := []struct {
		name      string
		sess      *v1alpha1.AgentSession
		wantBadge v1alpha1.OpeningBadge
	}{
		{"idle triggered humanless -> gate forces Done, not in_progress",
			triggeredSess("Idle", "github", "", nil), v1alpha1.OpeningBadgeDone},
		{"idle triggered WITH human starter -> in_progress (real conversation)",
			triggeredSess("Idle", "github", "user:alice", nil), v1alpha1.OpeningBadgeInProgress},
		{"idle conversational kind (no reporter) -> in_progress",
			triggeredSess("Idle", "fake", "", nil), v1alpha1.OpeningBadgeInProgress},
		{"running triggered -> in_progress (gate fires only on Idle)",
			triggeredSess("Running", "github", "", nil), v1alpha1.OpeningBadgeInProgress},
		{"idle triggered with a recorded outcome -> recorded outcome wins",
			triggeredSess("Idle", "github", "", &v1alpha1.PinnedMessageStatus{Badge: v1alpha1.OpeningBadgeProblemsFound}), v1alpha1.OpeningBadgeProblemsFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := pinnededit.Desired(tc.sess)
			require.True(t, ok)
			assert.Equal(t, tc.wantBadge, c.Badge)
		})
	}
}
