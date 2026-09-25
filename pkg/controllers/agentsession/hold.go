// pkg/controllers/agentsession/hold.go
//
// The forensic-hold park. Shaped after reconcileCredentialUpdatePark: called
// early in Reconcile, BEFORE the idle-sleep/archive/expiration sweep and
// BEFORE foldLifecycle/derivePhase, because the steady-state phase write
// would otherwise clobber the override (see reconcilePhase in phase.go), and
// because the archive/expiration sweeps can otherwise transition the session
// to a terminal phase the lifecycle machine's terminal-sticky guard would
// never let a later Held event move it off.
//
// It differs from that park in one deliberate way: this one reaps the
// session's pods (reconcileHold's final step, via reapSessionPods) — runner,
// bundle SpiceboxSessions, and detector/cosidecar pods — while leaving the
// workspace PVC and the AgentSession itself alone. The credential park leaves
// every pod alive because the runner is blocked in-process inside a meta tool
// call and tearing it down would destroy the blocked call. Here the runner is
// the thing being contained, so stopping it IS the point.
package agentsession

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// The plangate denial-streak tripper (pkg/authz/plangate/hold) creates
// SessionHold CRs in-process in the operator, via client.Client, stamping
// spec.source="tripper/plangate-denial-streak" (see SessionHoldSpec.Source).
// Its own package sits outside controller-gen's scanned paths (pkg/apis/...
// and pkg/controllers/...), so the `create` grant it needs is declared here
// instead, alongside this package's other SessionHold markers — read-only
// ones, since this reconciler itself never creates a SessionHold (see the
// get;list;watch marker on the Reconciler in controller.go).
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sessionholds,verbs=create

// sessionHoldRevokeKind matches sessionhold.Kind's Kind() ("session-hold"),
// the registered revocation.Invalidator that cancels a runner's root context
// on the session it names. Not imported directly (pkg/authz/revocation/kinds/
// sessionhold is a runner-side registration, not a controller dependency) --
// the wire string is the contract between the two, the same shape
// credentialRevokeKind (this package, passthrough_credhash.go) and
// toolOriginRevokeKind (pkg/controllers/mcpserver, pkg/controllers/settings)
// already use for their own Invalidators.
const sessionHoldRevokeKind = "session-hold"

// mapSessionHoldToSession re-enqueues the AgentSession a SessionHold names. A
// pure name projection: spec.sessionRef is 1:1, so no List is needed.
func mapSessionHoldToSession(_ context.Context, o client.Object) []reconcile.Request {
	h, ok := o.(*spiceboxv1alpha1.SessionHold)
	if !ok {
		return nil
	}
	if h.Spec.SessionRef.Namespace == "" || h.Spec.SessionRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: h.Spec.SessionRef.Namespace,
		Name:      h.Spec.SessionRef.Name,
	}}}
}

// holdsFor lists every SessionHold naming sess, namespace-scoped, mirroring
// awaitingCredentialUpdateRequestFor's shape: there is no index from session
// to its holds, and cardinality per namespace is small. Both activeHoldFor and
// releasedHoldFor filter this single List, so the List call -- and its
// fail-closed error handling -- lives in exactly one place.
func (r *Reconciler) holdsFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) ([]spiceboxv1alpha1.SessionHold, error) {
	var list spiceboxv1alpha1.SessionHoldList
	if err := r.Client.List(ctx, &list, client.InNamespace(sess.Namespace)); err != nil {
		return nil, fmt.Errorf("list SessionHolds in %s: %w", sess.Namespace, err)
	}
	var out []spiceboxv1alpha1.SessionHold
	for i := range list.Items {
		if list.Items[i].Spec.SessionRef.Name == sess.Name {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// activeHoldFor returns the first unreleased SessionHold naming sess, or nil.
func (r *Reconciler) activeHoldFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (*spiceboxv1alpha1.SessionHold, error) {
	holds, err := r.holdsFor(ctx, sess)
	if err != nil {
		return nil, err
	}
	for i := range holds {
		if !holds[i].IsReleased() {
			return &holds[i], nil
		}
	}
	return nil, nil
}

// releasedHoldFor returns the most recently created released SessionHold
// naming sess, or nil. "Most recently created" disambiguates a session held
// and released more than once: the newest released hold is the one whose
// release produced the session's CURRENT PhaseHeld-to-resume transition, an
// older cycle's hold having already been resolved and superseded.
func (r *Reconciler) releasedHoldFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (*spiceboxv1alpha1.SessionHold, error) {
	holds, err := r.holdsFor(ctx, sess)
	if err != nil {
		return nil, err
	}
	var newest *spiceboxv1alpha1.SessionHold
	for i := range holds {
		h := &holds[i]
		if !h.IsReleased() {
			continue
		}
		if newest == nil || h.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = h
		}
	}
	return newest, nil
}

// reconcileHold parks a session behind an unreleased SessionHold, called
// early in the main Reconcile — before the idle-sleep/archive/expiration
// sweep and before foldLifecycle/derivePhase — so its phase override is
// never immediately clobbered by the lifecycle-derived steady-state write,
// and so a hold cannot lose the race to a sweep that would otherwise
// transition the session to a terminal phase first.
//
// proceed=false means "stop, the caller should return res/err as-is" (a hold
// is active, forcing the park); proceed=true means "keep going" (nothing is
// holding this session, or the hold that was has been released).
func (r *Reconciler) reconcileHold(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (ctrl.Result, bool, error) {
	hold, err := r.activeHoldFor(ctx, sess)
	if err != nil {
		// Never fail open: a List error must not be read as "nothing is holding
		// this session", or a transient API blip would silently un-contain a
		// session a human deliberately froze.
		return ctrl.Result{}, false, err
	}
	if hold == nil {
		if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseHeld {
			// No unreleased hold names this session, yet the CR is still parked at
			// Held: derivePhase folds a lifecycle log whose last phase-changing
			// event is still Held (nothing has ever emitted Released), and would
			// keep re-projecting PhaseHeld forever without this. Resume it.
			return r.reconcileHoldRelease(ctx, sess)
		}
		return ctrl.Result{}, true, nil
	}

	logger := log.FromContext(ctx)

	// Stamp the observation ONCE. TrippedAt is set-on-first-observation, never
	// re-stamped, so repeated reconciles of an already-parked session stay
	// idempotent.
	if hold.Status.TrippedAt == nil {
		now := metav1.Now()
		hold.Status.TrippedAt = &now
		hold.Status.Phase = spiceboxv1alpha1.SessionHoldPhaseActive
		hold.Status.Containment = heldContainment(hold)
		if err := r.Client.Status().Update(ctx, hold); err != nil {
			logger.Info("SessionHold status stamp failed; will retry",
				"session", sess.Namespace+"/"+sess.Name, "hold", hold.Name, "err", err.Error())
			return ctrl.Result{RequeueAfter: 5 * time.Second}, false, nil
		}

		// Fast path: publish so a live runner's ap.revocation subscriber halts
		// its turn loop immediately, instead of waiting for reapSessionPods
		// below. Best-effort and latency-only -- reapSessionPods runs
		// regardless of whether this publish succeeds or whether any runner
		// ever receives it, so a failure here is logged, not returned; the pod
		// reap a few lines down is the actual containment guarantee.
		key := sess.Namespace + "/" + sess.Name
		if err := r.RevokePublisher.Emit(ctx, sessionHoldRevokeKind, key, sess.Namespace); err != nil {
			logger.Info("session-hold fast-path publish failed; containment still proceeds via pod reap",
				"session", sess.Namespace+"/"+sess.Name, "hold", hold.Name, "err", err.Error())
		}
	}

	// EVIDENCE BEFORE CONTAINMENT-CLEANUP. The pods must not be reaped until the
	// snapshot has landed, or the freeze destroys the thing it exists to keep.
	// hold.Status.SnapshotHandle is set only on success, so it also doubles as
	// the guard against launching a second snapshot Job on a later reconcile.
	if hold.Status.SnapshotHandle == "" {
		done, serr := r.ensureHoldSnapshot(ctx, sess, hold)
		switch {
		case serr != nil:
			// Containment still wins: park and reap anyway, but say so loudly
			// and on status. Proceeding silently as though we had snapshotted is
			// the one forbidden outcome.
			logger.Info("hold snapshot failed; parking without workspace evidence",
				"session", sess.Namespace+"/"+sess.Name, "hold", hold.Name, "err", serr.Error())
			r.setHoldContainment(ctx, hold, snapshotFailedContainment(serr), logger)
		case !done:
			// Job still running. Park the phase (containment is already in
			// force) and come back; do NOT reap yet.
			return ctrl.Result{RequeueAfter: 5 * time.Second}, false, nil
		}
	}

	// Project the Held phase through the lifecycle machine, exactly as the
	// other early-return paths do, so phase stays a projection of the log
	// rather than a string this file invents.
	if err := r.applyEvent(ctx, sess, lifecyclecore.Held{
		Reason:    hold.Spec.Reason,
		TrippedBy: string(hold.Spec.TrippedBy),
		At:        time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("apply Held for %s/%s: %w", sess.Namespace, sess.Name, err)
	}

	// proceed=false means the caller returns immediately, bypassing the single
	// applyStatus call at the tail of Reconcile -- so this branch, like
	// reconcileCredentialUpdatePark's park leg, must persist the projected
	// phase itself or the hold would never actually stick on the CR.
	if err := r.applyStatus(ctx, sess); err != nil {
		return ctrl.Result{}, false, err
	}

	// Reap the session's pods now that the hold is durably recorded:
	// reapSessionPods deletes the bundle SpiceboxSessions, the runner pod, and
	// any detector/cosidecar pods, while leaving the workspace PVC and the
	// AgentSession itself in place (mirrors reconcileSleep's identical
	// "park the session, drop its compute" call, sleep.go:79). Idempotent —
	// a repeat pass over an already-reaped session is a clean no-op.
	if _, err := r.reapSessionPods(ctx, sess); err != nil {
		return ctrl.Result{}, false, err // already logged inside reapSessionPods; retry
	}

	return ctrl.Result{}, false, nil
}

// reconcileHoldRelease resumes a session stranded at PhaseHeld once the hold
// that parked it has been released: it applies lifecyclecore.Released through
// the lifecycle machine, attributing the release to the human who cleared the
// card, so the fold this reconcile's later derivePhase performs projects the
// session off PhaseHeld instead of re-reading a log whose last phase-changing
// event is still Held.
//
// proceed is always true here — either a Released event was just appended and
// the rest of Reconcile should run so normal provisioning resumes, or there
// is no released hold to attribute the release to (a session manually forced
// into PhaseHeld with no SessionHold ever naming it), in which case there is
// nothing this method can do and the session is left exactly as it already
// was, same as if no hold had ever been observed.
func (r *Reconciler) reconcileHoldRelease(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (ctrl.Result, bool, error) {
	released, err := r.releasedHoldFor(ctx, sess)
	if err != nil {
		// Never fail open: a List error must not be read as "no released hold
		// exists", which would silently strand the session at Held forever
		// instead of retrying.
		return ctrl.Result{}, false, err
	}
	if released == nil {
		return ctrl.Result{}, true, nil
	}

	if err := r.applyEvent(ctx, sess, lifecyclecore.Released{
		ApprovedBy: string(released.Status.ReleasedBy),
		At:         time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("apply Released for %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	return ctrl.Result{}, true, nil
}

// holdSnapshotHandle addresses the freeze-time workspace snapshot.
//
// Qualifier exists for exactly this: ad-hoc snapshots that share a
// (SessionUID, TurnIndex). Keying it on the hold's NAME makes the operation
// idempotent — the same hold always resolves to the same handle, so a
// reconciler retry finds the existing Job instead of launching a second one.
func (r *Reconciler) holdSnapshotHandle(
	sess *spiceboxv1alpha1.AgentSession,
	hold *spiceboxv1alpha1.SessionHold,
) workspace.SnapshotHandle {
	return workspace.SnapshotHandle{
		SessionUID: string(sess.UID),
		// TurnIndex=-1 is the ad-hoc marker (see restart_pvc.go's identical use
		// for the clean-fork snapshot): a hold snapshot is not taken at a turn
		// boundary, so it renders as "adhoc" rather than a turn number.
		TurnIndex:        -1,
		Qualifier:        "hold-" + hold.Name,
		SnapshotStorePVC: podspec.SnapshotStoreClaimName(sess),
	}
}

// heldContainment and snapshotFailedContainment are the two things this
// reconciler ever has to say about a hold, as PURE FUNCTIONS of their inputs.
//
// Pure so that a repeated failure produces byte-identical text rather than an
// append onto whatever was there, which is what lets setHoldContainment's
// equality check short-circuit instead of growing the field without bound.
// pkg/controllers/sessionhold's recordCascadeFailure is written the same way,
// for the same reason — the two are the pair that used to loop.
func heldContainment(hold *spiceboxv1alpha1.SessionHold) string {
	return fmt.Sprintf("held: %s", hold.Spec.Reason)
}

func snapshotFailedContainment(err error) string {
	return "held; workspace snapshot FAILED: " + err.Error()
}

// setHoldContainment writes status.containment, and ONLY when it differs.
//
// The equality check is the second half of the two-field split and is not
// optional. Splitting the field stopped the two controllers ERASING each
// other's narrative; it does not stop them WAKING each other, because both
// still write the status of the same object and each write fires the other's
// watch. Without this check the AgentSession reconciler rewrote identical
// snapshot-failure text on every pass, and the SessionHold controller woke,
// re-derived its own answer, and wrote back.
//
// Containment is the field this reconciler owns; it must never write
// Determination, which belongs to the SessionHold controller.
//
// Errors are logged, never returned: containment has already been decided by
// this point and the caller is on its way to reap the pods. A failure to
// narrate must not stop the freeze — the same reasoning recordCascadeFailure
// applies on the other side.
func (r *Reconciler) setHoldContainment(ctx context.Context, hold *spiceboxv1alpha1.SessionHold, want string, logger logr.Logger) {
	if hold.Status.Containment == want {
		return
	}
	prior := hold.DeepCopy()
	hold.Status.Containment = want
	if err := r.Client.Status().Patch(ctx, hold, client.MergeFrom(prior)); err != nil {
		logger.Info("recording containment on SessionHold status failed",
			"hold", hold.Namespace+"/"+hold.Name, "err", err.Error())
	}
}

// ensureHoldSnapshot launches (or polls) the freeze-time workspace snapshot
// Job for hold, addressed by holdSnapshotHandle's stable handle so a
// reconciler retry finds the existing Job instead of launching a second one.
//
// Returns (true, nil) once the Job has completed and hold.Status.SnapshotHandle
// has been recorded; (false, nil) while the Job is still running, so the
// caller should requeue rather than reap; (false, err) when the snapshot
// could not be launched or failed terminally — the caller records that on
// hold.Status.Containment and reaps anyway, per the containment-wins
// failure policy.
func (r *Reconciler) ensureHoldSnapshot(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, hold *spiceboxv1alpha1.SessionHold) (bool, error) {
	if r.Snapshotter == nil {
		return false, fmt.Errorf("ensureHoldSnapshot: no Snapshotter configured on the operator")
	}

	src := workspace.PVCRef{Namespace: sess.Namespace, Name: podspec.WorkspaceClaimName(sess)}
	h := r.holdSnapshotHandle(sess, hold)

	// The Job runs in the session's namespace, referencing ap-snapshotter,
	// which the install bundle creates only in the operator's — see
	// workspace.EnsureSnapshotServiceAccount.
	if err := workspace.EnsureSnapshotServiceAccount(ctx, r.Client, sess.Namespace); err != nil {
		return false, fmt.Errorf("ensureHoldSnapshot: %w", err)
	}
	if err := r.Snapshotter.Snapshot(ctx, src, h); err != nil {
		return false, fmt.Errorf("ensureHoldSnapshot: launch snapshot: %w", err)
	}

	done, derr := r.Snapshotter.SnapshotDone(ctx, src, h)
	if errors.Is(derr, workspace.ErrSnapshotNotFound) {
		// The Snapshot call above just issued the create in this same pass, so
		// a NotFound from the probe is informer-cache lag, not a missing Job.
		return false, nil
	}
	if derr != nil {
		return false, fmt.Errorf("ensureHoldSnapshot: %w", derr)
	}
	if !done {
		return false, nil
	}

	hold.Status.SnapshotHandle = workspace.SnapshotJobName(h)
	if err := r.Client.Status().Update(ctx, hold); err != nil {
		return false, fmt.Errorf("ensureHoldSnapshot: record SnapshotHandle: %w", err)
	}
	return true, nil
}
