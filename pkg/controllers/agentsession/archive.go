// pkg/controllers/agentsession/archive.go
//
// reconcileArchive implements the operator's archive sweep: a channel-attached
// AgentSession in phase=Idle that has been idle longer than archiveAfter is
// transitioned to phase=Succeeded (Idle → Succeeded).
package agentsession

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// reconcileArchive evaluates the long-idle archive condition for a
// channel-attached AgentSession in phase=Idle. Returns:
//   - transitioned: true if the phase was patched Idle → Succeeded.
//   - requeueAfter: relative duration for the next deadline-driven reconcile (0 = no requeue needed).
//   - error: any patch error.
//
// Preconditions: caller has confirmed sess.Spec.InputChannel != nil and
// sess.Status.Phase == Idle.
func (r *Reconciler) reconcileArchive(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) (bool, time.Duration, error) {
	// Evidence outranks GC, same guard as reconcileSleep: archiving a held
	// session is a terminal, irreversible transition (the lifecycle machine's
	// terminal-sticky guard means a later Held event could never move the
	// session back off Succeeded), so a lookup error here must NOT be read as
	// "nothing is holding this session".
	//
	// This is a deliberate re-query, not a cache-miss to optimize away: a
	// SessionHold can be created after Reconcile's own earlier reconcileHold
	// check already ran this pass, so only a lookup taken at THIS sweep's own
	// moment can see it. Passing down the hold reconcileHold already resolved
	// would reintroduce the exact race this guard exists to close, and archive
	// winning that race cannot be undone.
	hold, herr := r.activeHoldFor(ctx, sess)
	if herr != nil {
		return false, 0, herr
	}
	if hold != nil {
		return false, 0, nil
	}

	archiveAfter := r.DefaultChannelArchiveAfter
	if class != nil && class.Spec.Channels != nil && class.Spec.Channels.ArchiveAfter.Duration > 0 {
		archiveAfter = class.Spec.Channels.ArchiveAfter.Duration
	}
	if archiveAfter == 0 {
		return false, 0, nil // disabled (operator flag set to 0 and no class override)
	}
	if sess.Status.LastIdleAt == nil {
		// Hasn't been stamped yet (this reconcile will populate it via the
		// stamping path; archive evaluation can wait one cycle).
		return false, 0, nil
	}
	deadline := sess.Status.LastIdleAt.Time.Add(archiveAfter)
	now := time.Now()
	if now.Before(deadline) {
		return false, deadline.Sub(now), nil
	}

	// Past deadline: archive. Record the sweep transition in the log, then set
	// the operator-owned terminal outcome. The explicit phase write stands
	// alongside the event: ArchiveSweep only advances a folded Idle state, and
	// until the runner publishes its IdleYield event the operator's fold does not
	// yet observe Idle — so the projection of ArchiveSweep is a no-op here. The
	// direct Succeeded write keeps archive correct in the meantime.
	patched := sess.DeepCopy()
	if err := r.applyEvent(ctx, patched, lifecyclecore.ArchiveSweep{}); err != nil {
		return false, 0, err
	}
	patched.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	patched.Status.FailureReason = ""
	finished := metav1.NewTime(now)
	patched.Status.FinishedAt = &finished
	patched.Status.LastIdleAt = nil
	conditions.Set(sess, &patched.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionIdle, Status: metav1.ConditionFalse,
		Reason:  spiceboxv1alpha1.ReasonAgentSessionArchived,
		Message: "session archived after long idle",
	})
	if err := agentstatus.WriteOwned(ctx, r.Client, patched, sess, spiceboxv1alpha1.OwnerOperator); err != nil {
		return false, 0, err
	}
	return true, 0, nil
}
