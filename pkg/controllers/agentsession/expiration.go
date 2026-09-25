// pkg/controllers/agentsession/expiration.go
//
// reconcileExpiration implements the operator's session-lifetime sweep: a
// non-terminal AgentSession whose wall-clock lifetime (now - status.StartedAt)
// exceeds budget.sessionExpiration is transitioned to phase=Failed
// (SessionExpired). This is the active enforcement path that reaps a session
// even while Idle/asleep; the runner's Budget.Check backstops live turns.
//
// Scope: the call site (controller.go, the Idle sleep/archive block) only
// invokes this sweep for channel-attached sessions currently Idle or asleep.
// A session parked in AwaitingRetry (provider error; pod exited) is NOT swept
// here — it has no runner to backstop it either, since the pod is down — but
// its lifetime is still bounded by its own retry TTL, and reconcileExpiration
// runs on the next wake (→ Pending → Running) if the session hasn't already
// retried successfully by then. This is an accepted, documented gap, not an
// oversight: AwaitingRetry burns no compute, so the worst case is a bounded
// delay before a stale session is reaped, not an unbounded one.
package agentsession

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// reconcileExpiration evaluates the wall-clock lifetime cap. Returns:
//   - transitioned: true if the phase was patched to Failed (SessionExpired).
//   - requeueAfter: relative duration until the deadline (0 = no requeue needed).
//   - error: any patch error.
//
// Precondition: caller has confirmed the session is non-terminal.
func (r *Reconciler) reconcileExpiration(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (bool, time.Duration, error) {
	// Evidence outranks GC, same guard as reconcileSleep/reconcileArchive:
	// failing a held session on its wall-clock cap is a terminal, irreversible
	// transition (the lifecycle machine's terminal-sticky guard means a later
	// Held event could never move the session back off Failed), so a lookup
	// error here must NOT be read as "nothing is holding this session".
	//
	// This is a deliberate re-query, not a cache-miss to optimize away: a
	// SessionHold can be created after Reconcile's own earlier reconcileHold
	// check already ran this pass, so only a lookup taken at THIS sweep's own
	// moment can see it. Passing down the hold reconcileHold already resolved
	// would reintroduce the exact race this guard exists to close, and this
	// sweep's outcome (Failed) is just as terminal-sticky and irreversible as
	// archive's.
	hold, herr := r.activeHoldFor(ctx, sess)
	if herr != nil {
		return false, 0, herr
	}
	if hold != nil {
		return false, 0, nil
	}

	es := sess.Status.EffectiveSettings
	if es == nil || es.Budget.SessionExpiration.Duration == 0 {
		return false, 0, nil // no cap set
	}
	if sess.Status.StartedAt == nil {
		return false, 0, nil // not started; nothing to expire yet
	}
	expiration := es.Budget.SessionExpiration.Duration
	deadline := sess.Status.StartedAt.Time.Add(expiration)
	now := r.now()
	if now.Before(deadline) {
		return false, deadline.Sub(now), nil
	}

	patched := sess.DeepCopy()
	if err := r.applyEvent(ctx, patched, lifecyclecore.Expired{}); err != nil {
		return false, 0, err
	}
	patched.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
	patched.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionExpired
	finished := metav1.NewTime(now)
	patched.Status.FinishedAt = &finished
	conditions.Set(sess, &patched.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionFailed, Status: metav1.ConditionTrue,
		Reason:  spiceboxv1alpha1.ReasonAgentSessionExpired,
		Message: fmt.Sprintf("session expired after %s (wall-clock lifetime cap)", expiration),
	})
	if err := agentstatus.WriteOwned(ctx, r.Client, patched, sess, spiceboxv1alpha1.OwnerOperator); err != nil {
		return false, 0, err
	}
	return true, 0, nil
}
