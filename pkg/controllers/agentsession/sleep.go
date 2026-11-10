// pkg/controllers/agentsession/sleep.go
//
// Idle-sleep decision for channel sessions: an Idle session that has sat
// beyond its configured sleepAfter grace without waking has its sandbox pods
// reaped (unlike a Failed/Succeeded terminal reap, the AgentSession itself
// stays Idle — a subsequent inbound message re-hydrates fresh pods).
package agentsession

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
)

// sandboxProvisioningDesired reports whether the reconciler should (re)create
// this session's bundle SpiceboxSessions on this pass. True only for Pending
// (booting) and Running (an active turn). Every parked phase stays false —
// Idle (slept or merely warm), AwaitingRetry, AwaitingCredentials, and
// AwaitingDecision (mid-turn: the runner already provisioned pods during the
// Pending pass that started the turn, so this only means "don't
// re-provision," not "tear down") — as do the terminal phases
// Succeeded/Failed. This only gates (re)creation; it never deletes — a
// session that already has pods from a prior Pending/Running pass keeps them
// until an explicit reap (sleep-due or terminal) tears them down. A parked
// session re-provisions on the next reconcile after it transitions back to
// Pending (wake, e.g.).
func sandboxProvisioningDesired(phase string) bool {
	return phase == spiceboxv1alpha1.AgentSessionPhasePending ||
		phase == spiceboxv1alpha1.AgentSessionPhaseRunning
}

// idleSleepDue reports whether an Idle channel session is past its sleep grace
// and not already slept. Pure so the boundary (disabled, no clock, already
// slept, within/past grace) is table-testable. remaining is the time until the
// sleep becomes due when within grace.
func idleSleepDue(sleptAt, lastIdleAt *metav1.Time, sleepAfter time.Duration, now time.Time) (bool, time.Duration) {
	if sleepAfter <= 0 || sleptAt != nil || lastIdleAt == nil {
		return false, 0
	}
	if remaining := sleepAfter - now.Sub(lastIdleAt.Time); remaining > 0 {
		return false, remaining
	}
	return true, 0
}

// reconcileSleep evaluates the idle-sleep condition for a channel-attached
// AgentSession in phase=Idle. Modeled on reconcileArchive. When past
// SleepAfter (and not already slept), it records the Sleep lifecycle event,
// stamps status.SleptAt, and reaps all session pods — the session stays Idle
// and wakeable. Returns (slept, requeueAfter, err). Preconditions: caller
// confirmed sess.Spec.InputChannel != nil, phase == Idle, and
// !shouldWake(sess).
//
// The admitter's reclaim request (AnnotationReclaimRequested) makes the sleep
// due now, whatever SleepAfter says — including when idle sleep is disabled —
// but never past the SessionHold guard. The annotation is removed once the
// session is slept.
func (r *Reconciler) reconcileSleep(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass) (bool, time.Duration, error) {
	// Evidence outranks GC. Archiving or reaping a held session would let the
	// forensic subject expire on a timer, which is exactly the failure mode that
	// ruled out reusing Failed + FailedSandboxReapGrace for this.
	//
	// This is a deliberate re-query, not a cache-miss to optimize away: a
	// SessionHold can be created after Reconcile's own earlier reconcileHold
	// check already ran this pass, so only a lookup taken at THIS sweep's own
	// moment can see it. Passing down the hold reconcileHold already resolved
	// would reintroduce the race this guard exists to close.
	hold, herr := r.activeHoldFor(ctx, sess)
	if herr != nil {
		return false, 0, herr
	}
	if hold != nil {
		return false, 0, nil
	}

	sleepAfter := r.DefaultSessionSleepAfter
	if class != nil && class.Spec.Channels != nil && class.Spec.Channels.SleepAfter.Duration > 0 {
		sleepAfter = class.Spec.Channels.SleepAfter.Duration
	}
	reclaim := reclaimRequested(sess)
	if reclaim && sess.Status.SleptAt != nil {
		// Already slept, so the request is fulfilled: a previous pass slept the
		// session but failed to remove the annotation. Remove it now; sleeping
		// again would reap nothing.
		return false, 0, r.clearReclaimRequest(ctx, sess)
	}
	due, remaining := idleSleepDue(sess.Status.SleptAt, sess.Status.LastIdleAt, sleepAfter, r.now())
	if reclaim && !due {
		log.FromContext(ctx).Info("sleeping idle session early: its AgentClass has a session waiting for the capacity",
			"session", sess.Namespace+"/"+sess.Name, "class", sess.Spec.Class,
			"reclaimRequested", sess.Annotations[spiceboxv1alpha1.AnnotationReclaimRequested])
		due, remaining = true, 0
	}
	if !due {
		return false, remaining, nil // remaining==0 when disabled/already-slept/no-clock
	}
	// Record the transition in the lifecycle log/state (Slept=true), then stamp
	// the CR marker and reap. applyEvent persists the folded state.
	if err := r.applyEvent(ctx, sess, lifecyclecore.Sleep{}); err != nil {
		return false, 0, fmt.Errorf("apply Sleep event: %w", err)
	}
	patched := sess.DeepCopy()
	// The reap below gives the session's place back to its line. Recorded in
	// THIS write, beside SleptAt: a wake clears SleptAt, so a release written
	// separately could be lost in between, leaving the session admitted with
	// no pods and its capacity counted twice. Decided before SleptAt is set,
	// which would hide a pre-admission session from the release.
	r.releaseAdmissionOnReclaim(patched)
	now := metav1.NewTime(r.now())
	patched.Status.SleptAt = &now
	if err := agentstatus.WriteOwned(ctx, r.Client, patched, sess, spiceboxv1alpha1.OwnerOperator); err != nil {
		return false, 0, fmt.Errorf("stamp SleptAt: %w", err)
	}
	if _, err := r.reapSessionPods(ctx, sess); err != nil {
		return false, 0, err // already logged inside; retry
	}
	// The annotation is metadata and SleptAt is status: two subresources, so
	// they cannot share the write above. It is removed only AFTER the status
	// write and the reap succeed, so a failure anywhere earlier leaves the
	// request in place for the retry; a failure here leaves a slept session
	// still carrying it, which the already-slept branch above clears on the
	// next pass without sleeping again.
	if reclaim {
		if err := r.clearReclaimRequest(ctx, sess); err != nil {
			return true, 0, err
		}
	}
	return true, 0, nil
}

// reclaimRequested reports whether the admitter has asked for this session to
// be slept early (AnnotationReclaimRequested).
func reclaimRequested(sess *spiceboxv1alpha1.AgentSession) bool {
	_, ok := sess.Annotations[spiceboxv1alpha1.AnnotationReclaimRequested]
	return ok
}

// clearStaleReclaimRequest removes a reclaim request from a session that is no
// longer Idle — it woke, or was held, before the sleep path acted on it. Left
// in place, it would sleep the session the moment it next went Idle, long
// after the admission line that asked for the capacity moved on. An Idle
// session's request is left for reconcileSleep.
func (r *Reconciler) clearStaleReclaimRequest(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle || !reclaimRequested(sess) {
		return nil
	}
	log.FromContext(ctx).Info("clearing a stale reclaim request: the session left Idle before it was slept",
		"session", sess.Namespace+"/"+sess.Name, "class", sess.Spec.Class, "phase", sess.Status.Phase)
	return r.clearReclaimRequest(ctx, sess)
}

// clearReclaimRequest removes AnnotationReclaimRequested with a metadata-only
// merge patch, sent on a fresh object carrying just the key so the server's
// response cannot overwrite the caller's in-memory status; the caller's copy
// drops the annotation to match.
func (r *Reconciler) clearReclaimRequest(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:null}}}`, spiceboxv1alpha1.AnnotationReclaimRequested)
	target := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: sess.Name, Namespace: sess.Namespace}}
	if err := r.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		log.FromContext(ctx).Info("removing the reclaim-requested annotation failed; retried on the next reconcile",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return fmt.Errorf("clear reclaim-requested annotation on %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	delete(sess.Annotations, spiceboxv1alpha1.AnnotationReclaimRequested)
	return nil
}
