// pkg/controllers/agentsession/maxduration.go
//
// reconcileRunDuration is the operator's backstop for budget.maxDuration. The
// runner enforces the cap itself: between turns, and as a deadline on in-flight
// work (a watchdog on its run clock cancels the in-flight LLM call and tool
// calls once active run-time reaches the cap). So in a healthy session this
// never fires. It exists for the runner that has stopped enforcing anything:
// one wedged somewhere the cancellation cannot reach, whose run-time flusher
// still reports a status.runDuration climbing far past the cap while the
// session sits in Running indefinitely.
//
// A Running session whose runDuration exceeds its enforced cap plus
// runDurationGrace is moved to Failed (BudgetExceeded). From there the ordinary
// terminal path takes over: the Failed reap tears down the runner and its pods
// after the configured grace, exactly as for any other failed session.
//
// The cap compared against is status.enforcedMaxDuration, the value the
// current runner pod read at start and reports alongside runDuration, NOT
// status.effectiveSettings. Reconcile re-resolves effectiveSettings from the
// live class and session budget every pass, so after a class's maxDuration is
// lowered it no longer describes the cap the running runner holds, and
// comparing against it would fail every healthy session already past the new
// value. A session with no enforcedMaxDuration (no cap, or a runner from
// before the field existed) is skipped rather than measured against the live
// cap for the same reason.
//
// It runs early in Reconcile, ahead of the AgentClass and settings gates: its
// inputs are all on the session's own status, and a wedged runner is exactly
// the session most likely to also be sitting behind a class that went
// Valid=False.
//
// No requeue is scheduled: the input is status.runDuration, and the runner's
// periodic runDuration patch is itself an AgentSession update that re-triggers
// this reconcile. A runner whose flusher stopped too is a dead pod, which the
// pod-failure handling covers.
//
// What this does NOT bound: the inputs are runner-written (see runDuration and
// enforcedMaxDuration in the admission webhook's runnerWritableStatusFields),
// so a runner that under-reports or freezes runDuration, or clears its
// reported cap, is never caught here. The backstop targets a stuck runner
// whose flusher still reports honestly, not a hostile one. A runner able to
// lie here already holds the session's credentials and tool access, so
// overspending its own time budget is not the escalation that matters; a
// runner-independent bound would key on an operator- or kubelet-written signal
// (the runner pod's start time) instead.
package agentsession

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/triggeranswer"
)

// runDurationGrace is how far past the enforced cap a session's reported
// run-time may go before the operator steps in. The runner's own enforcement
// starts AT the cap (its watchdog cancels the LLM call and stateless/readonly
// tools then, and lets stateful tools drain for up to runner.BudgetStatefulDrain, 5m,
// in pkg/agent/runner/budget.go), so the grace has to cover that drain, the
// cancelled calls unwinding, the runner's 30s runDuration flush interval, and
// its failure write, so the operator never races a healthy runner to the
// verdict. Keep the two in step.
const runDurationGrace = 10 * time.Minute

// reconcileRunDuration fails a Running session whose reported active run-time
// is past status.enforcedMaxDuration + runDurationGrace. Returns whether it
// transitioned.
func (r *Reconciler) reconcileRunDuration(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (bool, error) {
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseRunning {
		return false, nil
	}
	if sess.Status.EnforcedMaxDuration == nil || sess.Status.EnforcedMaxDuration.Duration <= 0 ||
		sess.Status.RunDuration == nil {
		return false, nil
	}
	maxDuration := sess.Status.EnforcedMaxDuration.Duration
	ran := sess.Status.RunDuration.Duration
	if ran <= maxDuration+runDurationGrace {
		return false, nil
	}

	// Same guard as reconcileExpiration, for the same reason: Failed is
	// terminal-sticky, so a session under an evidence hold must not be failed
	// out from under it, and only a lookup taken now can see a hold created
	// since this reconcile started.
	hold, err := r.activeHoldFor(ctx, sess)
	if err != nil {
		return false, err
	}
	if hold != nil {
		return false, nil
	}

	msg := fmt.Sprintf("run-time %s exceeded budget.maxDuration %s by more than %s; the runner did not end the session itself, so the operator did",
		ran.Round(time.Second), maxDuration, runDurationGrace)
	log.FromContext(ctx).Info("failing session past maxDuration (operator backstop)",
		"session", sess.Namespace+"/"+sess.Name, "runDuration", ran.String(),
		"maxDuration", maxDuration.String(), "grace", runDurationGrace.String())

	if err := r.failSessionByOperator(ctx, sess, lifecyclecore.RunDurationExceeded{},
		spiceboxv1alpha1.ReasonAgentSessionBudget, msg); err != nil {
		return false, err
	}
	// The session was Running, so whatever its trigger asked for was still in
	// progress, and its wedged runner is not going to answer. Only after the
	// write persisted, so a transition that did not land publishes nothing;
	// best-effort and read-before-write, so a verdict already posted is kept.
	reason := spiceboxv1alpha1.ReasonAgentSessionBudget
	r.concludeTriggerStatusOnOperatorFailure(ctx, sess, reason, triggeranswer.StoppedSummary(reason))
	return true, nil
}
