package toolcall

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// ReconcileFailureBudget bounds how long a ToolCall may keep failing its
// reconcile before the call is finished outright with the last error as its
// terminal reason.
//
// The two ends it has to satisfy pull in opposite directions. A control-plane
// blip, a memory-store restart or a cache lag must be ridden out, so the
// budget is long enough to cover several backoff attempts. But something is
// WAITING on this call — a turn, an agent, a person — and every second past
// the point where progress became impossible is a second they spend on a
// session that looks healthy and will never move. Two minutes is longer than
// any transient this controller has been observed to hit and far shorter than
// the forever it used to wait.
const ReconcileFailureBudget = 2 * time.Minute

// now is the reconciler's clock; tests inject a fixed one via Reconciler.Now.
func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// surfaceReconcileFailure records a failing reconcile on the ToolCall itself
// and, once the failures have repeated past ReconcileFailureBudget, finishes
// the call rather than requeueing it again.
//
// A ToolCall that keeps failing is not an operator-log problem: a runner is
// blocked on its terminal condition, and through the runner an agent and a
// person. Returning the error forever leaves all three with nothing — the
// session stays Running, the pods stay healthy, and the failure is visible
// only to whoever happens to be reading operator logs. So the FIRST failure
// already lands on the object as ReconcileRetrying=True carrying the error,
// and a streak that outlasts the budget becomes Failed=True with the same
// error as its message. The runner turns that into an IsError tool result, and
// the agent relays the reason to whoever asked.
//
// Returns (res, rerr) unchanged whenever the call should simply be retried.
func (r *Reconciler) surfaceReconcileFailure(ctx context.Context, nn types.NamespacedName, res ctrl.Result, rerr error) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !countsTowardFailureBudget(ctx, rerr) {
		return res, rerr
	}

	// Re-read: the failing reconcile may have mutated its own copy, and the
	// error may have come from a write that lost a race.
	var tc spiceboxv1alpha1.ToolCall
	cont, err := apreconcile.LoadInto(ctx, r.Client, nn, &tc)
	if err != nil {
		logger.Info("toolcall: could not re-read a ToolCall to record its failing reconcile; the failure is being retried",
			"toolcall", nn.String(), "readErr", err.Error(), "err", rerr.Error())
		return res, rerr
	}
	if !cont {
		// Deleted underneath us. Nothing is waiting on it any more.
		return ctrl.Result{}, nil
	}
	if terminalCondition(&tc) != "" {
		return res, rerr
	}

	// A permanent error produces the identical failure on every retry, so
	// spending the budget waiting for a recovery that cannot come only delays
	// the reason the waiting caller is owed — the very "healthy-looking session
	// that never moves" the budget exists to shorten. Finish now, on the first
	// occurrence, instead of opening a streak. The canonical case is an
	// append-only audit conflict from WaitPreDispatchSnapshot: a snapshot record
	// whose ID already holds different content (a prior session run's snapshot,
	// at a turnCount this run reused) can never be re-written.
	if isPermanentReconcileError(rerr) {
		return r.finishReconcileAsFailed(ctx, &tc, res, rerr,
			fmt.Sprintf("reconcile cannot make progress and will not recover on retry; last error: %v", rerr))
	}

	conditions.Set(&tc, &tc.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.ToolCallConditionReconcileRetrying,
		Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonToolCallReconcileError, Message: rerr.Error(),
		// Stamped from the reconciler's own clock so the streak's age and the
		// budget it is measured against come from one source.
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	// Set preserves LastTransitionTime while the status stays True, so this is
	// the age of the whole streak, not of the latest attempt.
	streak := conditions.Find(tc.Status.Conditions, spiceboxv1alpha1.ToolCallConditionReconcileRetrying)
	elapsed := r.now().Sub(streak.LastTransitionTime.Time)

	if elapsed <= ReconcileFailureBudget {
		if uerr := r.Client.Status().Update(ctx, &tc); uerr != nil && !apierrors.IsConflict(uerr) {
			logger.Info("toolcall: recording a failing reconcile on the ToolCall failed; the failure is still being retried",
				"toolcall", nn.String(), "writeErr", uerr.Error(), "err", rerr.Error())
		}
		return res, rerr
	}

	return r.finishReconcileAsFailed(ctx, &tc, res, rerr,
		fmt.Sprintf("reconcile has been failing for %s and cannot make progress; last error: %v",
			elapsed.Round(time.Second), rerr))
}

// isPermanentReconcileError reports whether err will recur identically on every
// retry, so surfacing it now is strictly better than spending the failure
// budget waiting for a recovery that cannot come. An append-only conflict — a
// Put that would change an existing audit/transcript entry — is deterministic
// in exactly this way: the stored entry never changes and the record the
// reconcile recomputes never changes.
func isPermanentReconcileError(err error) bool {
	return errors.Is(err, memory.ErrAppendOnlyConflict)
}

// finishReconcileAsFailed writes the terminal Failed condition (with message),
// records FinishedAt, and closes any Running condition, then returns a result
// that stops the requeue. If the status write itself fails the call is NOT
// actually finished, so it returns the original (res, rerr) to be retried
// rather than reporting a resolution no waiting caller can read.
func (r *Reconciler) finishReconcileAsFailed(ctx context.Context, tc *spiceboxv1alpha1.ToolCall, res ctrl.Result, rerr error, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	r.setFailed(tc, spiceboxv1alpha1.ReasonToolCallReconcileFailed, message)
	finished := metav1.NewTime(r.now())
	tc.Status.FinishedAt = &finished
	if hasTrueCondition(tc, spiceboxv1alpha1.ToolCallConditionRunning) {
		conditions.Set(tc, &tc.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.ToolCallConditionRunning, Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ReasonExecClosed,
		})
	}
	if uerr := r.Client.Status().Update(ctx, tc); uerr != nil {
		logger.Info("toolcall: could not finish a ToolCall whose reconcile cannot make progress",
			"toolcall", tc.Namespace+"/"+tc.Name, "writeErr", uerr.Error(), "err", rerr.Error())
		return res, rerr
	}
	logger.Info("toolcall: finished a ToolCall whose reconcile could not make progress",
		"toolcall", tc.Namespace+"/"+tc.Name, "err", rerr.Error())
	return ctrl.Result{}, nil
}

// clearReconcileFailure ends a failing streak after a reconcile succeeds, so
// the next failure is measured from itself. Without it a call that blipped
// once early would run the rest of its life on a budget already part-spent,
// and a much later, unrelated blip would finish it on its first attempt.
//
// Only ever touches ReconcileRetrying: a ToolCall that reached a terminal
// condition did so through a SUCCESSFUL reconcile, and clearing anything else
// here would erase the outcome that reconcile just recorded.
func (r *Reconciler) clearReconcileFailure(ctx context.Context, nn types.NamespacedName) {
	logger := log.FromContext(ctx)
	var tc spiceboxv1alpha1.ToolCall
	cont, err := apreconcile.LoadInto(ctx, r.Client, nn, &tc)
	if err != nil {
		logger.Info("toolcall: could not re-read a ToolCall to clear its failing-reconcile streak",
			"toolcall", nn.String(), "err", err.Error())
		return
	}
	if !cont || !hasTrueCondition(&tc, spiceboxv1alpha1.ToolCallConditionReconcileRetrying) {
		return
	}
	conditions.Set(&tc, &tc.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.ToolCallConditionReconcileRetrying,
		Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonToolCallReconcileRecovered,
		Message: fmt.Sprintf("reconcile recovered; previous error: %s",
			conditions.Find(tc.Status.Conditions, spiceboxv1alpha1.ToolCallConditionReconcileRetrying).Message),
		LastTransitionTime: metav1.NewTime(r.now()),
	})
	if uerr := r.Client.Status().Update(ctx, &tc); uerr != nil && !apierrors.IsConflict(uerr) {
		logger.Info("toolcall: clearing a failing-reconcile streak failed; a later failure may be measured from the old streak",
			"toolcall", nn.String(), "err", uerr.Error())
	}
}

// countsTowardFailureBudget reports whether err is evidence that this ToolCall
// cannot make progress.
//
// Two kinds of error are not. A cancelled context is the process shutting
// down, and finishing every in-flight call on a rollout would be worse than
// the hang. An optimistic-concurrency conflict is the API server saying
// "re-read and try again" — the work never failed, and letting a busy object
// spend its budget on writes that were always going to be re-driven would
// finish calls that were fine.
func countsTowardFailureBudget(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	return !apierrors.IsConflict(err)
}
