// pkg/controllers/agentsession/retry_ttl.go
//
// checkRetryTTL enforces the bounded AwaitingRetry window: it limits how long
// the operator waits for a user to click Retry before failing the session with
// RetryTimeout, and it reconciles the CR phase with the signed lifecycle log
// when the retry budget is exhausted.
//
// Two sub-tasks run on the same reconcile path:
//  1. Lifecycle reconciliation: if the fold of the signed log shows
//     PhaseFailed (retry budget exhausted by the runner), apply that terminal
//     phase even though WriteAwaitingRetry wrote AwaitingRetry to the CR — the
//     lifecycle state machine is the authoritative source of truth.
//  2. TTL gate: stamp AnnotationAwaitingRetrySince on first observation, then
//     requeue-after the remainder; when elapsed ≥ retryTTL emit RetryTTLExpired
//     so the core's Transition moves the session to Failed[RetryTimeout].
package agentsession

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// defaultRetryTTL is the window a session stays in AwaitingRetry before
// the operator gives up and moves it to Failed[RetryTimeout]. 30 minutes
// gives the user plenty of time to click Retry while bounding resource
// contention from zombie AwaitingRetry sessions.
const defaultRetryTTL = 30 * time.Minute

// checkRetryTTL is called when the session's CR phase is AwaitingRetry and
// the wake annotation is NOT set (shouldWake returned false). It returns
// (done=true, result) when the caller must immediately return, or (false, {})
// when the session is within the TTL window and should fall through to the
// normal isTerminalForPodLifecycle park path.
//
// The returned result is intentionally returned WITHOUT an error companion —
// callers must write it as `return result, nil` or `return result, <err>`.
func (r *Reconciler) checkRetryTTL(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
) (done bool, result ctrl.Result, err error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	// Sub-task 1: lifecycle reconciliation. If the signed log fold already
	// shows PhaseFailed (e.g. retry budget exhausted by the runner before the
	// operator reconciled), apply the terminal phase now. Without this step
	// WriteAwaitingRetry's direct CR patch would permanently disagree with the
	// lifecycle state machine — the session would never leave AwaitingRetry.
	if r.LifecycleMemory != nil {
		state, foldErr := r.foldLifecycle(ctx, sess)
		if foldErr != nil {
			return true, ctrl.Result{}, foldErr
		}
		if state.Phase == lifecyclecore.PhaseFailed {
			// The log already recorded the terminal event (ProviderError past
			// the cap). Just project the terminal phase onto the CR and finish.
			sess.Status.Phase = lifecyclecore.Project(state).Phase
			sess.Status.FailureReason = state.FailureReason
			now := metav1.Now()
			sess.Status.FinishedAt = &now
			conditions.SetFalse(sess, &sess.Status.Conditions,
				spiceboxv1alpha1.AgentSessionConditionAwaitingRetry,
				spiceboxv1alpha1.ReasonAgentSessionRetryBudgetExhausted, "")
			r.clearRetrySinceAnnotation(ctx, sess)
			logger.Info("retry budget exhausted (lifecycle fold); transitioning to Failed",
				"reason", state.FailureReason)
			return true, ctrl.Result{}, nil
		}
	}

	// Sub-task 2: TTL gate.
	now := r.now()
	sinceStr, hasSince := sess.Annotations[spiceboxv1alpha1.AnnotationAwaitingRetrySince]
	if !hasSince || sinceStr == "" {
		// First observation: stamp the entry timestamp and requeue after the full TTL.
		if patchErr := r.stampRetrySinceAnnotation(ctx, sess, now); patchErr != nil {
			return true, ctrl.Result{}, patchErr
		}
		logger.Info("AwaitingRetry: stamped entry timestamp; requeueing after TTL",
			"ttl", defaultRetryTTL.String())
		return true, ctrl.Result{RequeueAfter: defaultRetryTTL}, nil
	}

	since, parseErr := time.Parse(time.RFC3339Nano, sinceStr)
	if parseErr != nil {
		// Corrupt annotation — re-stamp with the current time to self-heal.
		logger.Info("AwaitingRetry: corrupt awaiting-retry-since annotation; re-stamping",
			"raw", sinceStr, "err", parseErr.Error())
		if patchErr := r.stampRetrySinceAnnotation(ctx, sess, now); patchErr != nil {
			return true, ctrl.Result{}, patchErr
		}
		return true, ctrl.Result{RequeueAfter: defaultRetryTTL}, nil
	}

	elapsed := now.Sub(since)
	if elapsed < defaultRetryTTL {
		remaining := defaultRetryTTL - elapsed
		logger.V(1).Info("AwaitingRetry: TTL not yet elapsed; requeueing",
			"elapsed", elapsed.String(), "remaining", remaining.String())
		return true, ctrl.Result{RequeueAfter: remaining}, nil
	}

	// TTL elapsed: emit RetryTTLExpired → Failed[RetryTimeout].
	logger.Info("AwaitingRetry TTL elapsed; transitioning to Failed[RetryTimeout]",
		"since", sinceStr, "elapsed", elapsed.String())
	if evErr := r.applyEvent(ctx, sess, lifecyclecore.RetryTTLExpired{}); evErr != nil {
		return true, ctrl.Result{}, evErr
	}
	now2 := metav1.Now()
	// Set Phase explicitly so the status write reflects Failed even if the
	// lifecycle fold inside applyEvent doesn't emit a ProjectStatus (e.g.
	// when the fold is already terminal). Mirrors the cap-path's explicit
	// phase assignment above so the two terminal paths are symmetric.
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
	sess.Status.FinishedAt = &now2
	sess.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionRetryTimeout
	conditions.SetFalse(sess, &sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionAwaitingRetry,
		spiceboxv1alpha1.ReasonAgentSessionRetryTimeout, "retry TTL elapsed")
	r.clearRetrySinceAnnotation(ctx, sess)
	return true, ctrl.Result{}, nil
}

// stampRetrySinceAnnotation patches AnnotationAwaitingRetrySince onto the
// AgentSession metadata without changing status. Uses a MergePatch so the
// patch races only against concurrent metadata writers (common), not status
// writers (e.g. the runner) — keeping it separate from applyStatus avoids
// a spurious status conflict.
func (r *Reconciler) stampRetrySinceAnnotation(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	now time.Time,
) error {
	ts := now.UTC().Format(time.RFC3339Nano)
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{%q:%q}}}`,
		spiceboxv1alpha1.AnnotationAwaitingRetrySince, ts,
	))
	if err := r.Client.Patch(ctx, sess,
		client.RawPatch(types.MergePatchType, patch),
	); err != nil {
		return fmt.Errorf("stamp awaiting-retry-since annotation: %w", err)
	}
	// Keep the in-memory object consistent so callers see the new annotation.
	if sess.Annotations == nil {
		sess.Annotations = make(map[string]string)
	}
	sess.Annotations[spiceboxv1alpha1.AnnotationAwaitingRetrySince] = ts
	return nil
}

// clearRetrySinceAnnotation removes AnnotationAwaitingRetrySince from the
// session's metadata. Best-effort: a failure is logged but does not block the
// transition — the annotation is cosmetic once the session is no longer in
// AwaitingRetry, and a stale value only causes an immediate-requeue on the
// next reconcile (which then finds the phase is no longer AwaitingRetry and
// skips the TTL path).
func (r *Reconciler) clearRetrySinceAnnotation(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{%q:null}}}`,
		spiceboxv1alpha1.AnnotationAwaitingRetrySince,
	))
	if err := r.Client.Patch(ctx, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sess.Name,
			Namespace: sess.Namespace,
		},
	}, client.RawPatch(types.MergePatchType, patch)); err != nil {
		logger.Info("clearRetrySinceAnnotation: patch failed (best-effort)",
			"err", err.Error())
	}
	delete(sess.Annotations, spiceboxv1alpha1.AnnotationAwaitingRetrySince)
}
