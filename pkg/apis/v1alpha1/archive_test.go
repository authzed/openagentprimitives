package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestArchivedBySweep(t *testing.T) {
	archived := []metav1.Condition{{
		Type: AgentSessionConditionIdle, Status: metav1.ConditionFalse,
		Reason: ReasonAgentSessionArchived, LastTransitionTime: metav1.Now(),
	}}
	cases := []struct {
		name string
		sess *AgentSession
		want bool
	}{
		{"swept while idle: parked", &AgentSession{Status: AgentSessionStatus{Conditions: archived}}, true},
		{"agent finished: not parked", &AgentSession{Status: AgentSessionStatus{}}, false},
		{"superseded parent: done, its child owns the thread", &AgentSession{
			Status: AgentSessionStatus{Conditions: archived, SupersededBy: "child"}}, false},
		{"nil session: not parked", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ArchivedBySweep(tc.sess))
		})
	}
}
