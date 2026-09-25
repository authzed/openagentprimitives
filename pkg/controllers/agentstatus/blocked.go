package agentstatus

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// notReadyGates are the POSITIVE readiness gates whose False genuinely means
// "this session has not started", ordered most-actionable first.
//
// The order is the whole point: a wedged session usually has several gates
// False at once (scheduling failed, so bundles are not ready, so the runner
// never came up), and only the first one is a cause — the rest are its
// consequences. Reporting BundlesReady's "waiting for capacity" when
// SandboxScheduling already said "Insufficient memory" buries the actionable
// fact under its own downstream effect.
var notReadyGates = []string{
	spiceboxv1alpha1.AgentSessionConditionFailed,
	spiceboxv1alpha1.AgentSessionConditionCredentialsReady,
	spiceboxv1alpha1.AgentSessionConditionClassResolved,
	spiceboxv1alpha1.AgentSessionConditionSettingsAccepted,
	spiceboxv1alpha1.AgentSessionConditionSandboxScheduling,
	spiceboxv1alpha1.AgentSessionConditionBundlesReady,
	spiceboxv1alpha1.AgentSessionConditionSkillBundlesIntegrity,
	spiceboxv1alpha1.AgentSessionConditionRunnerReady,
}

// FirstNotReadyGate returns the reason/message of the most informative positive
// readiness gate that is False on sess, and whether one was found at all.
//
// It deliberately does NOT fall back to "any False condition". Many AgentSession
// conditions are negative-polarity, where False (or absent) is the HEALTHY
// state: ScopeReviewPending (Resolved), ToolCallGated (NoDenials), Idle,
// AwaitingRetry, and the *ApprovalPending / PermissionRequestPending gates. A
// slow-to-start session routinely has those already resolved to False while its
// positive gates are merely absent — a catch-all then reports nonsense like
// "couldn't start (Resolved)" for a session that goes on to run fine.
//
// Shared by webd's chat health watcher (which turns it into a terminal
// "couldn't start" notice) and channelsd's startup caption (which turns it into
// a live thread status), so the ranking is defined exactly once.
func FirstNotReadyGate(sess *spiceboxv1alpha1.AgentSession) (reason, message string, ok bool) {
	if sess == nil {
		return "", "", false
	}
	for _, want := range notReadyGates {
		if c := meta.FindStatusCondition(sess.Status.Conditions, want); c != nil && c.Status == metav1.ConditionFalse {
			return c.Reason, c.Message, true
		}
	}
	return "", "", false
}

// ReasonDetail joins a condition reason with its human message ("SecretMissing:
// secret foo not found"), collapsing to just the reason when there is no
// message and to just the message when there is no reason.
func ReasonDetail(reason, message string) string {
	switch {
	case reason != "" && message != "":
		return reason + ": " + message
	case reason != "":
		return reason
	default:
		return message
	}
}
