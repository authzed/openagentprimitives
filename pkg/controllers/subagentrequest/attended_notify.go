package subagentrequest

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/inboxwake"
)

// Task 7 of the agent-builder delegation framework: while Task 7's own
// routing (attended_watch.go's counterpart in pkg/channels/channelsd/pipeline)
// sends a human's turns to a live `attended` child instead of its watching
// parent, THIS file is the other half -- what the parent is told once the
// child ends. The parent is never blocked in a tool call while it watches (it
// is not the party polling a SubagentRequest the way a task/chat delegation's
// parent is), so there is no return value to hand it: the only way to tell it
// anything is to put a turn in its own inbox and force it awake, the same two
// effects channelsd's own applyWake produces for an ordinary human message.

// attendedStoppedLine is the fixed line an attended child's watching parent
// receives when the child ends any way OTHER than succeeding: a premature
// delete (a person stopped it), a crash, or its own conversation timing out
// unanswered. The Copy rule: no infra ("namespace"/"CR"/"session"/"kubectl")
// -- the parent reads this exactly as it would any other inbound turn.
const attendedStoppedLine = "The test was stopped."

// attendedCompletedLine is the fixed line for the one outcome that is not a
// stop: the child finished on its own.
const attendedCompletedLine = "The test finished."

// attendedLineFor picks the fixed copy for phase. Anything other than
// Succeeded reads as a stop, including a crash or an unanswered timeout —
// from the point of view of a person watching the child's own thread go
// quiet, "the test was stopped" is the accurate, honest reading of all of
// them, and a THIRD line distinguishing "crashed" from "stopped" would be
// exactly the kind of infra-flavored precision the Copy rule instructs
// against.
func attendedLineFor(phase string) string {
	if phase == v1.SubagentRequestPhaseSucceeded {
		return attendedCompletedLine
	}
	return attendedStoppedLine
}

// notifyAttendedParent appends line to the watching parent's own inbox as a
// fixed inbound turn, then forces a wake — the SAME wake contract
// channelsd's own applyWake uses (stamp the wake-requested-at annotation,
// then publish the identical KindUserMessage NATS nudge). The mechanics live
// in pkg/controllers/inboxwake, shared with the Workshop controller's own
// "person testing a built agent" notifications rather than reinvented here.
//
// Unlike attended_watch.go's mirrorToAttendedParent (an ORDINARY child turn,
// gated through WakeCredit like any other cross-agent mention), this wake is
// FORCED, with no WakeCredit gate: resolve() and finalizeAttended each reach
// a terminal outcome for a given SubagentRequest AT LEAST once, ordinarily
// exactly once — see finalizeAttended's own doc for the rare crash-window
// exception, which resolve() shares for the same reason (it notifies before
// its own terminal write). Either way this is never a repeatable ping a compromised or
// malfunctioning child could invoke at will (nothing calls this from an
// ordinary inbound turn), so there is no abuse for a budget to bound, only a
// bounded, cosmetic duplicate in the worst case.
//
// Best-effort in what it returns. inboxwake.Notify logs and returns a nil
// ParentMemory/PublishInteraction, a failed read/write, a wake that failed
// behind a line that landed (inboxwake.WakeError), or a parent that has
// finished (inboxwake.ErrSessionOver), and both callers proceed anyway — the
// terminal write and the finalizer removal must not be undone by a
// notification that could not be delivered. Nothing here waits for a better
// moment: the line rides the "inbox" role, so it lands whatever the parent's
// own loop is doing at the time.
func (r *Reconciler) notifyAttendedParent(ctx context.Context, sr *v1.SubagentRequest, line string) error {
	return inboxwake.Notify(ctx, r.Client, r.reader(), r.ParentMemory, inboxwake.Publish(r.PublishInteraction), r.clock,
		sr.Spec.Parent.Namespace, sr.Spec.Parent.Name, line)
}

// attendedNotifyOutcome names what a failed notifyAttendedParent actually
// cost, for the log line at each call site. A wake-only failure is not a
// parent that was not told: the line IS in its transcript and it reads it the
// next time anything wakes it, so an operator reading this log must not go
// looking for a notice that is sitting right there. Every other failure means
// the line never landed at all.
func attendedNotifyOutcome(err error) string {
	var wakeErr *inboxwake.WakeError
	if errors.As(err, &wakeErr) {
		return "the line is in the parent's transcript but the wake did not fire; the parent reads it when it next wakes"
	}
	return "the parent was not told"
}

// finalizeAttended is the deletion arm for a request that ever carried
// FinalizerSubagentRequest (attended mode only — Reconcile's EnsureFinalizer
// call site). It fires on EVERY deletion of such a request, including
// reclaimTerminal's own routine cleanup delete once a request has already
// resolved — so the one thing that decides whether this is NEW information
// for the parent is sr.IsTerminal(): resolve() (via deny/fail/the Succeeded
// arm) already notified the parent the moment this request first reached a
// terminal phase, so a request that is ALREADY terminal here is being
// reclaimed, not stopped, and gets no second notification. A request that is
// NOT yet terminal here was deleted out from under a live delegation — a
// genuine stop — and gets attendedStoppedLine, but only if a child actually
// existed to be watched by a human at all (sr.Status.ChildRef != nil): a
// request stuck mid-creation, deleted before its child ever existed, never
// put a human in front of anything to report ending.
//
// The notification is AT-LEAST-once here, not exactly-once, and that is a
// deliberate, bounded choice rather than an oversight. The finalizer-removal
// Update is wrapped in RetryOnConflict (re-Get, re-remove, re-Update) so an
// ordinary optimistic-lock conflict — cache lag, a concurrent writer, the
// routine reason a finalizer removal needs a retry at all — resolves inside
// THIS call and never re-enters finalizeAttended with the notify-guard still
// true. What RetryOnConflict cannot close is a process crash in the window
// between a successful notifyAttendedParent and the retry loop's own
// successful persist: the finalizer is still present on next reconcile, the
// guard re-evaluates true, and the parent receives a second, duplicate fixed
// line. That residual is irreducible by construction — the notify is a
// side effect on a DIFFERENT object (the parent's own memory + wake
// annotation), which cannot be made atomic with this object's own
// finalizer-removal write — and it is deliberately accepted rather than
// "fixed" by marking notified-before-removal: marking first would trade a
// rare, harmless, cosmetic duplicate line for a possible SILENTLY MISSED
// "the test was stopped" notice (mark persists, then the process crashes
// before the removal — the parent never finds out), which is the strictly
// worse failure mode for a message whose entire point is to tell someone
// their test ended. Do not add a durable "notified" marker to close this
// gap; the residual is the correct trade, not a bug to chase.
func (r *Reconciler) finalizeAttended(ctx context.Context, sr *v1.SubagentRequest) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sr, v1.FinalizerSubagentRequest) {
		return ctrl.Result{}, nil
	}
	if !sr.IsTerminal() && sr.Status.ChildRef != nil {
		// The notice goes out before the finalizer is released, so a crash
		// between the two costs a duplicate line rather than a lost one. A
		// failure does not hold the finalizer: the delete must be allowed to
		// complete, and a stop nobody could be told about is reported here
		// rather than retried forever.
		if err := r.notifyAttendedParent(ctx, sr, attendedStoppedLine); err != nil {
			log.FromContext(ctx).Info("attended stop: "+attendedNotifyOutcome(err)+"; the finalizer is released anyway so the delete completes",
				"request", sr.Namespace+"/"+sr.Name,
				"parent", sr.Spec.Parent.Namespace+"/"+sr.Spec.Parent.Name, "err", err.Error())
		}
	}
	key := types.NamespacedName{Namespace: sr.Namespace, Name: sr.Name}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1.SubagentRequest
		if err := r.Get(ctx, key, &cur); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // already released by a prior successful pass
			}
			return err
		}
		controllerutil.RemoveFinalizer(&cur, v1.FinalizerSubagentRequest)
		return r.Update(ctx, &cur)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove attended finalizer from SubagentRequest %s/%s: %w", sr.Namespace, sr.Name, err)
	}
	return ctrl.Result{}, nil
}
