// sessionsForUserIdentityChange must re-enqueue the subject's ENTIRE
// non-terminal set on a UserIdentity change, not just sessions parked in
// AwaitingCredentials: a Ready/Running session whose passthrough credential is
// replaced or revoked would otherwise keep its cached token valid until some
// unrelated reconcile happened to fire. Terminal sessions and other subjects'
// sessions stay excluded.
package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestSessionsForUserIdentityChange(t *testing.T) {
	subject := "user:alice"
	sess := func(name, phase, subj string) spiceboxv1alpha1.AgentSession {
		return spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   "default",
				Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: subj},
			},
			Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
		}
	}
	items := []spiceboxv1alpha1.AgentSession{
		sess("parked", spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, subject),
		sess("running", spiceboxv1alpha1.AgentSessionPhaseRunning, subject),
		sess("idle", spiceboxv1alpha1.AgentSessionPhaseIdle, subject),
		sess("failed", spiceboxv1alpha1.AgentSessionPhaseFailed, subject),
		sess("succeeded", spiceboxv1alpha1.AgentSessionPhaseSucceeded, subject),
		sess("other-user-running", spiceboxv1alpha1.AgentSessionPhaseRunning, "user:bob"),
	}

	got := sessionsForUserIdentityChange(items, subject)

	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	assert.ElementsMatch(t, []string{"parked", "running", "idle"}, names,
		"non-terminal sessions of the subject are re-enqueued; terminal (Failed/Succeeded) + other-subject sessions are excluded")
}
