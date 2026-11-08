// pkg/controllers/agentsession/maxduration.go
//
// reconcileRunDuration is the operator's backstop for budget.maxDuration. The
// runner enforces the cap itself, between turns and as a deadline on each LLM
// call, so in a healthy session this never fires. It exists for the runner that
// has stopped enforcing anything: one wedged in a call that never returns, whose
// run-time flusher still reports a status.runDuration climbing far past the cap
// while the session sits in Running indefinitely.
//
// A Running session whose runDuration exceeds maxDuration plus
// runDurationGrace is moved to Failed (BudgetExceeded). From there the ordinary
// terminal path takes over: the Failed reap tears down the runner and its pods
// after the configured grace, exactly as for any other failed session.
//
// No requeue is scheduled: the input is status.runDuration, and the runner's
// periodic runDuration patch is itself an AgentSession update that re-triggers
// this reconcile. A runner whose flusher stopped too is a dead pod, which the
// pod-failure handling covers.
//
// What this does NOT bound: the input is runner-written (see runDuration in
// the admission webhook's runnerWritableStatusFields), so a runner that
// under-reports or freezes runDuration is never caught here. The backstop
// targets a stuck runner whose flusher still reports honestly, not a hostile
// one. A runner able to lie here already holds the session's credentials and
// tool access, so overspending its own time budget is not the escalation that
// matters; a runner-independent bound would key on an operator- or
// kubelet-written signal (the runner pod's start time) instead.
package agentsession

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// runDurationGrace is how far past maxDuration a session's reported run-time
// may go before the operator steps in. It covers the runner's own enforcement
// (which fires at the cap) plus its 30s runDuration flush interval and the
// failure write, so the operator never races a healthy runner to the verdict.
const runDurationGrace = 10 * time.Minute

// reconcileRunDuration fails a Running session whose reported active run-time
// is past maxDuration + runDurationGrace. Returns whether it transitioned.
func (r *Reconciler) reconcileRunDuration(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (bool, error) {
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseRunning {
		return false, nil
	}
	es := sess.Status.EffectiveSettings
	if es == nil || es.Budget.MaxDuration.Duration <= 0 || sess.Status.RunDuration == nil {
		return false, nil
	}
	maxDuration := es.Budget.MaxDuration.Duration
	ran := sess.Status.RunDuration.Duration
	if ran <= maxDuration+runDurationGrace {
		return false, nil
	}

	// Same guard as reconcileExpiration, for the same reason: Failed is
	// terminal-sticky, so a session under an evidence hold must not be failed
	// out from under it, and only a lookup taken now can see a hold created
	// since this reconcile's earlier reconcileHold.
	hold, err := r.activeHoldFor(ctx, sess)
	if err != nil {
		return false, err
	}
	if hold != nil {
		return false, nil
	}

	msg := fmt.Sprintf("run-time %s exceeded budget.maxDuration %s; the runner stopped enforcing its budget, so the operator ended the session",
		ran.Round(time.Second), maxDuration)
	log.FromContext(ctx).Info("failing session past maxDuration (operator backstop)",
		"session", sess.Namespace+"/"+sess.Name, "runDuration", ran.String(),
		"maxDuration", maxDuration.String(), "grace", runDurationGrace.String())

	patched := sess.DeepCopy()
	if err := r.applyEvent(ctx, patched, lifecyclecore.RunDurationExceeded{}); err != nil {
		return false, err
	}
	patched.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
	patched.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionBudget
	finished := metav1.NewTime(r.now())
	patched.Status.FinishedAt = &finished
	conditions.Set(sess, &patched.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionFailed, Status: metav1.ConditionTrue,
		Reason:  spiceboxv1alpha1.ReasonAgentSessionBudget,
		Message: msg,
	})
	if err := agentstatus.WriteOwned(ctx, r.Client, patched, sess, spiceboxv1alpha1.OwnerOperator); err != nil {
		return false, err
	}
	return true, nil
}
