// pkg/controllers/agentsession/agentidentity_watch_test.go
//
// Task 6: an AgentIdentity change (credential rotation/revocation) must
// promptly re-run reconcileCredentialGrants for the AgentSessions it may
// affect, rather than waiting for the next periodic resync.
//
// sessionsForAgentIdentityChange is namespace-scoped rather than matched
// against a specific session field: the identity that actually drives
// reconcileCredentialGrants is the session's AgentClass's class-level
// default (sessionRuntimeIdentity in credential_grants.go resolves
// ac.Spec.AgentIdentity, not sess.Spec.AgentIdentity), so a precise
// per-session match would require loading each session's AgentClass inside
// the watch's map func. AgentIdentity changes are rare and
// reconcileCredentialGrants is idempotent (a no-op write when the desired
// grant set is unchanged), so every NON-TERMINAL AgentSession in the
// AgentIdentity's namespace is re-enqueued — regardless of its
// spec.agentIdentity value. Terminal sessions (per isTerminalPhase) and
// sessions in a different namespace are excluded.
package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestSessionsForAgentIdentityChange(t *testing.T) {
	const aiNamespace = "default"
	sess := func(name, namespace, phase, agentIdentity string) spiceboxv1alpha1.AgentSession {
		return spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec:   spiceboxv1alpha1.AgentSessionSpec{AgentIdentity: agentIdentity},
			Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
		}
	}
	items := []spiceboxv1alpha1.AgentSession{
		// Non-terminal, in-namespace: enqueued regardless of spec.agentIdentity
		// (empty = inherits the AgentClass default, which is the common case;
		// "agent-identity-b" = an unrelated session-level override) — the whole
		// point of the fix is that this field no longer gates the match.
		sess("running-no-override", aiNamespace, spiceboxv1alpha1.AgentSessionPhaseRunning, ""),
		sess("idle-with-override", aiNamespace, spiceboxv1alpha1.AgentSessionPhaseIdle, "agent-identity-b"),
		sess("parked", aiNamespace, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, ""),
		// Terminal, in-namespace: excluded (no live credential consumer).
		sess("failed", aiNamespace, spiceboxv1alpha1.AgentSessionPhaseFailed, ""),
		sess("succeeded", aiNamespace, spiceboxv1alpha1.AgentSessionPhaseSucceeded, ""),
		// Non-terminal, different namespace: excluded by the helper's own
		// namespace check (defense in depth beyond the map func's InNamespace list).
		sess("other-namespace-running", "other-ns", spiceboxv1alpha1.AgentSessionPhaseRunning, ""),
	}

	got := sessionsForAgentIdentityChange(items, aiNamespace)

	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	assert.ElementsMatch(t, []string{"running-no-override", "idle-with-override", "parked"}, names,
		"every non-terminal in-namespace session is re-enqueued regardless of spec.agentIdentity; terminal + other-namespace sessions are excluded")
}
