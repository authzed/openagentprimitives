package agentsession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestShouldWake_ArchivedSucceededIsWakeable pins that the archive sweep parks
// a session rather than ending it: a fresh inbound must be able to wake it.
// A genuinely-finished Succeeded session stays terminal.
func TestShouldWake_ArchivedSucceededIsWakeable(t *testing.T) {
	archivedCond := []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionIdle, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived, LastTransitionTime: metav1.Now(),
	}}
	cases := []struct {
		name         string
		phase        string
		conds        []metav1.Condition
		supersededBy string
		want         bool
	}{
		{"archived-Succeeded + wake annotation: wakes", spiceboxv1alpha1.AgentSessionPhaseSucceeded, archivedCond, "", true},
		{"agent-complete Succeeded: stays terminal", spiceboxv1alpha1.AgentSessionPhaseSucceeded, nil, "", false},
		{"Idle + wake annotation: wakes (unchanged)", spiceboxv1alpha1.AgentSessionPhaseIdle, nil, "", true},
		{"superseded parent: stays terminal", spiceboxv1alpha1.AgentSessionPhaseSucceeded, archivedCond, "child-session", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						spiceboxv1alpha1.AnnotationWakeRequestedAt: time.Now().Format(time.RFC3339Nano),
					},
				},
				Status: spiceboxv1alpha1.AgentSessionStatus{
					Phase:        tc.phase,
					Conditions:   tc.conds,
					SupersededBy: tc.supersededBy,
				},
			}
			assert.Equal(t, tc.want, shouldWake(sess))
		})
	}
}
