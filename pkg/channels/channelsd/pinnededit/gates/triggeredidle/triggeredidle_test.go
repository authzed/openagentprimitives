package triggeredidle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	// Register the channel kinds the gate classifies: github owns a
	// trigger-status reporter, fake does not.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
)

func sessWith(inputKind, startedBy string) *v1alpha1.AgentSession {
	s := &v1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}}
	if inputKind != "" {
		s.Spec.InputChannel = &v1alpha1.ChannelBinding{Name: "in", Kind: inputKind}
	}
	if startedBy != "" {
		s.Annotations = map[string]string{v1alpha1.AnnotationStartedByCanonicalID: startedBy}
	}
	return s
}

// The gate forces a terminal badge ONLY for a triggered (trigger-status
// reporter) session with NO human starter — the case that would otherwise park
// Idle and read "in progress" forever with no one to resume it. A human-started
// session (a real conversation) and a conversational kind (no reporter) both
// abstain, so their Idle behavior is untouched.
func TestTerminalBadgeForIdle(t *testing.T) {
	cases := []struct {
		name      string
		sess      *v1alpha1.AgentSession
		wantBadge v1alpha1.OpeningBadge
		wantOK    bool
	}{
		{"github trigger, no human starter -> Done", sessWith("github", ""), v1alpha1.OpeningBadgeDone, true},
		{"github trigger WITH human starter -> abstain (real conversation)", sessWith("github", "user:alice"), "", false},
		{"conversational kind (no reporter) -> abstain", sessWith("fake", ""), "", false},
		{"no input channel (kubectl) -> abstain", sessWith("", ""), "", false},
		{"nil session -> abstain", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			badge, ok := gate{}.TerminalBadgeForIdle(tc.sess)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantBadge, badge)
		})
	}
}

// The gate registers itself under a stable key.
func TestGateRegistered(t *testing.T) {
	assert.Equal(t, "triggered-session-idle-concluded", gate{}.Key())
}
