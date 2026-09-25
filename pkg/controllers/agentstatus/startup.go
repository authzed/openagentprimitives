package agentstatus

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// StartupCaption derives the live "what is in the way" caption for a session
// that has not started yet, from its conditions: FriendlyGate over the
// FirstNotReadyGate ranking, so the cause is named and its consequences are
// not. ok=false means "say nothing" — leave whatever caption is up alone.
//
// Shared by channelsd's StartupStatusWatcher (which captions the session's
// channel thread) and webd's chat health watcher (which captions the browser
// shell), so a person sees the same sentence on both surfaces, and so there
// is no second copy of "why is this stuck" to drift.
//
// It is deliberately quiet in three cases:
//   - The session has STARTED. From then on the runner owns the status
//     surface (update_status, plan updates, tool progress); republishing
//     startup text over live turn state would clobber it.
//   - Credentials are genuinely outstanding. The credential watcher's own
//     caption — which carries the link the person needs — owns that window,
//     and a competing caption would fight it every tick.
//   - Nothing is False yet. An ordinarily slow start has no blocker to
//     report, and each surface's generic "starting…" placeholder stands.
func StartupCaption(sess *spiceboxv1alpha1.AgentSession) (text, short string, ok bool) {
	if sess == nil {
		return "", "", false
	}
	if spiceboxv1alpha1.AgentSessionPhaseStarted(sess.Status.Phase) {
		return "", "", false
	}
	if c := meta.FindStatusCondition(sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionCredentialsReady); c != nil && c.Status == metav1.ConditionFalse {
		return "", "", false
	}
	reason, message, found := FirstNotReadyGate(sess)
	if !found {
		return "", "", false
	}
	text, short = FriendlyGate(reason, message)
	return text, short, true
}
