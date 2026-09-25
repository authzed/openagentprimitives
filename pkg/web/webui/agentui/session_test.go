package agentui_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
)

const (
	testNamespace  = "workshop"
	testAgentClass = "demo-agent"
	testSubject    = "user:alice"
)

// session builds a minimal AgentSession fixture for namespace/class
// testNamespace/testAgentClass, with the given name/phase/creation-offset.
func session(name, phase string, createdAgo time.Duration) spiceboxv1alpha1.AgentSession {
	return spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-createdAgo)),
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: testAgentClass},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: phase,
		},
	}
}

// archivedSucceeded builds a Succeeded session that the operator's archive
// sweep parked (pod reaped, not really "done") — v1alpha1.ArchivedBySweep's
// contract: Idle condition present with Reason=Archived, SupersededBy empty.
func archivedSucceeded(name string, createdAgo time.Duration) spiceboxv1alpha1.AgentSession {
	s := session(name, spiceboxv1alpha1.AgentSessionPhaseSucceeded, createdAgo)
	s.Status.Conditions = []metav1.Condition{{
		Type:   spiceboxv1alpha1.AgentSessionConditionIdle,
		Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived,
	}}
	return s
}

// TestResolveSession table-drives every ladder rung ResolveSession can reach
// from a single, already-interact-authorized AgentSession's own phase/
// wake-eligibility — the function takes no Reader, no subject, and does no
// I/O (see session.go's doc comment on why: it is keyed to the ONE named
// session the caller already resolved and authorized, never a search across
// the namespace for "the most recent session of this class").
func TestResolveSession(t *testing.T) {
	tests := []struct {
		name       string
		sess       spiceboxv1alpha1.AgentSession
		wantBranch agentui.Branch
		wantName   string
	}{
		{
			name:       "Running -> Attached (live)",
			sess:       session("sess-running", spiceboxv1alpha1.AgentSessionPhaseRunning, time.Minute),
			wantBranch: agentui.Attached,
			wantName:   "sess-running",
		},
		{
			name:       "Pending -> Attached (still live, no wake needed)",
			sess:       session("sess-pending", spiceboxv1alpha1.AgentSessionPhasePending, time.Minute),
			wantBranch: agentui.Attached,
			wantName:   "sess-pending",
		},
		{
			name:       "AwaitingDecision -> Attached (blocked on approval, still live)",
			sess:       session("sess-decision", spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision, time.Minute),
			wantBranch: agentui.Attached,
			wantName:   "sess-decision",
		},
		{
			name:       "AwaitingCredentials -> Attached (waiting on user, not asleep)",
			sess:       session("sess-creds", spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, time.Minute),
			wantBranch: agentui.Attached,
			wantName:   "sess-creds",
		},
		{
			name:       "Idle -> Asleep (slept; this page performs no wake)",
			sess:       session("sess-idle", spiceboxv1alpha1.AgentSessionPhaseIdle, time.Hour),
			wantBranch: agentui.Asleep,
			wantName:   "sess-idle",
		},
		{
			name:       "AwaitingRetry -> Asleep (pod exited, wake eligible)",
			sess:       session("sess-retry", spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, time.Hour),
			wantBranch: agentui.Asleep,
			wantName:   "sess-retry",
		},
		{
			name:       "archive-swept Succeeded -> Asleep (parked, not finished)",
			sess:       archivedSucceeded("sess-archived", 24*time.Hour),
			wantBranch: agentui.Asleep,
			wantName:   "sess-archived",
		},
		{
			name:       "genuinely-completed Succeeded -> Ended (this session is terminal; no replacement is created)",
			sess:       session("sess-done", spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Hour),
			wantBranch: agentui.Ended,
			wantName:   "",
		},
		{
			name:       "Failed, including wall-clock expiry -> Ended (expiration transitions straight to Failed)",
			sess:       session("sess-failed", spiceboxv1alpha1.AgentSessionPhaseFailed, time.Hour),
			wantBranch: agentui.Ended,
			wantName:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := agentui.ResolveSession(&tc.sess)
			assert.Equal(t, tc.wantBranch, got.Branch)
			assert.Equal(t, tc.wantName, got.SessionName)
		})
	}
}
