package sessionhold

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// cascadeName deterministically names the SessionHold this controller creates
// for child within originating's cascade, so a retried Create lands on the
// SAME object (AlreadyExists, not a duplicate hold) rather than one named by
// GenerateName — mirroring pkg/authz/plangate/hold/tripper.go's trip, which
// documents the identical reasoning for its own hold: a retry of an
// interrupted cascade must not mint a second SessionHold for a session that
// already has one.
func cascadeName(originating *spiceboxv1alpha1.SessionHold, child *spiceboxv1alpha1.AgentSession) string {
	return fmt.Sprintf("cascade-%s-%s", originating.Name, child.Name)
}

// cascadeHold fans originating out across its entire delegation subtree: one
// SessionHold per descendant of originating's own session, each owner-ref'd
// to ITS OWN session (never to the root, never to originating) so k8s garbage
// collection tracks each cascaded hold to the session it actually freezes,
// not to the hold that caused it to exist.
//
// A per-session hold rather than a wider revocation envelope is deliberate:
// ap.revocation carries no per-session subject (every runner in the
// namespace receives every session-hold revoke and no-ops on a key
// mismatch), so freezing one session that way would freeze every concurrent
// session in the namespace instead of just the one closure this fans out to.
//
// Idempotent: cascadeName is deterministic, and AlreadyExists on Create is
// treated as success — reconciling the same originating hold twice creates no
// second set. It is also called on EVERY reconcile of an Active hold (see
// Reconcile), not just the first: a delegation started after the hold went
// Active is caught on the hold's next reconcile rather than never, and the
// AgentSession watch in SetupWithManager exists to make that "next reconcile"
// prompt rather than accidental.
//
// A cascaded hold never itself cascades: originating carrying
// v1alpha1.LabelCascadeOf means IT is a cascaded hold, and this method
// returns immediately. Guarding on the label rather than parsing
// Spec.Source's "cascade/" prefix keeps the check independent of that
// string's exact spelling — the label is the thing releaseCascade also reads,
// so both directions of the cascade agree on the same signal.
func (r *Reconciler) cascadeHold(ctx context.Context, originating *spiceboxv1alpha1.SessionHold, logger logr.Logger) error {
	if _, cascaded := originating.Labels[spiceboxv1alpha1.LabelCascadeOf]; cascaded {
		return nil
	}

	sessKey := client.ObjectKey{Namespace: originating.Spec.SessionRef.Namespace, Name: originating.Spec.SessionRef.Name}
	var sess spiceboxv1alpha1.AgentSession
	if err := r.Client.Get(ctx, sessKey, &sess); err != nil {
		return fmt.Errorf("sessionhold: cascadeHold: get AgentSession %s: %w", sessKey, err)
	}

	descendants, err := spiceboxv1alpha1.DescendantsOf(ctx, r.Client, &sess)
	if err != nil {
		return fmt.Errorf("sessionhold: cascadeHold: resolve descendants of %s: %w", sessKey, err)
	}

	for i := range descendants {
		child := &descendants[i]
		h := &spiceboxv1alpha1.SessionHold{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cascadeName(originating, child),
				Namespace: child.Namespace,
				Labels:    map[string]string{spiceboxv1alpha1.LabelCascadeOf: originating.Name},
				// Owner-ref'd to the CHILD -- its own session, not the root and not
				// originating. This is load-bearing for GC: a hold owner-ref'd to the
				// wrong session would either never be cleaned up when its actual
				// target session is deleted, or be deleted out from under a session
				// that is still alive.
				OwnerReferences: cosidecar.OwnerRef(child),
			},
			Spec: spiceboxv1alpha1.SessionHoldSpec{
				SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: child.Namespace, Name: child.Name},
				Reason: fmt.Sprintf("cascaded from a hold on %s/%s: %s",
					originating.Spec.SessionRef.Namespace, originating.Spec.SessionRef.Name, originating.Spec.Reason),
				Source: "cascade/" + originating.Name,
			},
		}
		if err := r.Client.Create(ctx, h); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("sessionhold: cascadeHold: create cascaded SessionHold %s/%s: %w", h.Namespace, h.Name, err)
		}
		logger.Info("sessionhold: cascaded hold to descendant",
			"originating", originating.Namespace+"/"+originating.Name, "session", child.Namespace+"/"+child.Name, "hold", h.Name)
	}
	return nil
}

// releaseCascade releases every SessionHold this controller cascaded from
// originating (holds labelled v1alpha1.LabelCascadeOf == originating.Name) —
// called from Decide once the originating hold's own release has been
// decided, so ONE approval clears the whole subtree instead of requiring a
// separate click per descendant.
//
// *** THIS IS THE ANTI-ONE-WAY-DOOR STEP; READ THIS BEFORE THE REST OF THE
// FILE. *** A cascaded hold is owner-ref'd to its own child session (see
// cascadeHold), never to originating, so k8s garbage collection never removes
// it just because originating is released -- releasing the root does nothing
// to the children unless something explicitly cascades that release too.
// Without this method, cascadeHold alone would make the hold WORSE than the
// un-cascaded original: instead of one frozen session with one release path,
// a held subtree would become N frozen sessions, each needing its own
// separate approval discovered independently, with the human who clicked
// "release" on the root believing the whole subtree was freed.
//
// This method ONLY writes Phase/Determination/ReleasedBy — it never deletes.
// A cascaded hold's CR is left in place after release, exactly like every
// OTHER released hold in this codebase (a manual hold, a tripper's), and is
// garbage-collected the ordinary way: by its own owner-ref, when its session
// is eventually deleted. This is deliberate, not an oversight: the
// AgentSession reconciler resumes a held session by reading this hold's
// Status BACK — reconcileHoldRelease (pkg/controllers/agentsession/hold.go)
// via releasedHoldFor — and lifecycle.Transition's Released case
// (pkg/agent/session/lifecycle/transition.go) is the ONLY way off PhaseHeld.
// Deleting a cascaded hold ahead of that read — even after setting Released,
// even "once it looked safe" — would remove the one durable record the OTHER
// controller needs to finish resuming the session, racing an asynchronous
// watch this controller cannot observe the other side of. An earlier version
// of this method paired the release with a confirm-then-delete step: delete
// only once the child's own status.phase was observed to have left Held.
// That was rejected because the confirmation window was itself still a
// race — reconcileHold lists holds at the top of its pass and stamps
// PhaseHeld at the end, so a delete landing in that window still destroys
// the record the child needs. Never deleting removes the race entirely
// instead of narrowing it.
//
// A failure partway (one child's patch errors) returns immediately, leaving
// any NOT-YET-PROCESSED cascaded hold exactly as it was and propagating the
// error out through Decide -- which, by construction (see Decide), leaves
// originating's OWN Phase at Active rather than Released. That is
// deliberate: a release that cascades to only part of the subtree is not a
// completed release, the same reasoning clearStandingApprovals already
// applies to its own write. A retry re-Lists the (now smaller) set of
// not-yet-released cascaded holds and finishes the job; an already-released
// child is filtered out below (IsReleased), so the retry is a clean
// continuation, not a redo.
func (r *Reconciler) releaseCascade(ctx context.Context, originating *spiceboxv1alpha1.SessionHold, approvedBy identity.Subject, logger logr.Logger) error {
	var list spiceboxv1alpha1.SessionHoldList
	if err := r.Client.List(ctx, &list,
		// Scoped from originating's SESSION namespace, not originating's own
		// object namespace -- the same field cascadeHold derives a cascaded
		// hold's namespace from (child.Namespace, which is always a member of
		// originating's session's own namespace). The two object-namespace
		// fields agree in every case that exists today (a SessionHold always
		// lives in its session's namespace), but deriving both from the same
		// field makes that a structural guarantee rather than a convention two
		// call sites merely happen to follow.
		client.InNamespace(originating.Spec.SessionRef.Namespace),
		client.MatchingLabels{spiceboxv1alpha1.LabelCascadeOf: originating.Name},
	); err != nil {
		return fmt.Errorf("sessionhold: releaseCascade: list cascaded holds for %s: %w", originating.Name, err)
	}

	for i := range list.Items {
		child := &list.Items[i]
		if child.IsReleased() {
			continue
		}
		prior := child.DeepCopy()
		child.Status.Phase = spiceboxv1alpha1.SessionHoldPhaseReleased
		child.Status.Determination = "released: cascaded from " + originating.Name
		child.Status.ReleasedBy = approvedBy
		if err := r.Client.Status().Patch(ctx, child, client.MergeFrom(prior)); err != nil {
			return fmt.Errorf("sessionhold: releaseCascade: patch release for cascaded hold %s/%s: %w",
				child.Namespace, child.Name, err)
		}
		logger.Info("sessionhold: released a cascaded hold",
			"originating", originating.Namespace+"/"+originating.Name, "hold", child.Namespace+"/"+child.Name,
			"session", child.Spec.SessionRef.Namespace+"/"+child.Spec.SessionRef.Name)
	}
	return nil
}

// recordCascadeFailure records a failed cascadeHold attempt on
// hold.Status.Determination, so an operator inspecting the SessionHold
// (`kubectl get sessionhold -o yaml`, or any log scrape) sees that some
// descendants may still be running unheld. The release CARD itself is
// unchanged by this — sessionrelease.BuildCard never reads Determination,
// so a human clicking the card sees nothing about the cascade's outcome —
// only Status and the log line right above this call carry the failure.
//
// Mirrors pkg/controllers/agentsession/hold.go's reconcileHold snapshot-
// failure branch ("Containment still wins: park and reap anyway, but say so
// loudly and on status") and never returns an error for the identical
// reason: the caller (Reconcile) must still publish the release card
// regardless of whether recording the failure itself succeeds -- a human
// locked out because BOTH the cascade AND its own failure-record failed
// would be worse than one that is merely under-documented on status.
//
// The recorded message is a pure function of cascadeErr's text, not an
// append onto whatever Determination already held, so a repeated failure on
// every retry produces a byte-identical write (Patch computes an empty diff)
// instead of growing without bound.
//
// THE EQUALITY CHECK IS ALSO HALF OF A CROSS-CONTROLLER FIX, and that is the
// load-bearing reason to keep it. Determination is written here; Containment
// is written by the AgentSession reconciler. Both live on the same object's
// status, so every write here fires that reconciler's watch and vice versa.
// The two used to share ONE field with no check on the other side: this wrote
// the cascade failure, that woke and overwrote it with the snapshot failure,
// this saw its own text was gone and wrote again — a loop in which each
// controller also erased the other's report, in exactly the doubly-degraded
// state where an operator needs both. Splitting the field stopped the
// erasure; skipping the unchanged write is what stops the wakeups.
func (r *Reconciler) recordCascadeFailure(ctx context.Context, hold *spiceboxv1alpha1.SessionHold, cascadeErr error, logger logr.Logger) {
	want := fmt.Sprintf("cascade FAILED: %s -- descendants may still be running unheld", cascadeErr.Error())
	if hold.Status.Determination == want {
		return
	}
	prior := hold.DeepCopy()
	hold.Status.Determination = want
	if err := r.Client.Status().Patch(ctx, hold, client.MergeFrom(prior)); err != nil {
		logger.Info("sessionhold: recording cascade failure on status failed",
			"hold", hold.Namespace+"/"+hold.Name, "err", err.Error())
	}
}

// mapSessionToAncestorHolds re-enqueues every UNRELEASED SessionHold covering
// one of sess's ancestors whenever sess itself changes -- chiefly on CREATE,
// the case a delegation starting a moment after an ancestor was already held
// would otherwise slip through unheld until something else happened to
// requeue that hold. Registered in SetupWithManager as a Watches(...) on
// AgentSession; see that method's doc for why a watch was chosen over
// refusing the delegation outright. "Unreleased" rather than "Active"
// deliberately: a hold not yet stamped Active by reconcileHold still needs
// this watch's re-enqueue the same as one already Active does, since either
// shape names a session that should freeze sess's newly-delegated child too.
//
// The walk is bounded by v1alpha1.WalkAncestors' own depth guard
// (MaxLineageWalk), and a legal roster is capped at 3 hops
// (MaxDelegationDepth), so this is one namespaced List of SessionHolds plus
// ONE short ancestor walk per AgentSession event -- sess's own ancestor set
// is resolved ONCE below and every candidate hold is tested for membership
// in it, rather than re-walking the identical chain from sess once per hold.
// Not free, but small and bounded, mirroring this package's other holds
// Lists (activeHoldFor, holdsFor in pkg/controllers/agentsession/hold.go)
// which make the same "cardinality per namespace is small, no index needed"
// call.
//
// A mapping function has no error return to propagate through, so a List or
// walk failure is logged (never silently dropped, per AGENTS.md's
// no-silent-errors rule) and this returns no requests for that event -- and
// unlike cascadeHold itself, there is NO backstop for a missed one. Once a
// hold's card is published, nothing further changes that hold's object (a
// human's click aside), so nothing else re-enqueues it: this watch is the
// SOLE prompt trigger for "a child delegated after an ancestor was already
// held". A List or walk failure on the one event that would have caught a
// given child means that child runs uncontained until either a human decides
// the hold, or the manager's own SyncPeriod (10h; see
// pkg/controllers/credentialupdaterequest's identical citation of it as a
// last resort, not a plan) eventually re-lists everything.
func (r *Reconciler) mapSessionToAncestorHolds(ctx context.Context, o client.Object) []reconcile.Request {
	sess, ok := o.(*spiceboxv1alpha1.AgentSession)
	if !ok {
		return nil
	}
	logger := log.FromContext(ctx)

	var holds spiceboxv1alpha1.SessionHoldList
	if err := r.Client.List(ctx, &holds, client.InNamespace(sess.Namespace)); err != nil {
		logger.Info("sessionhold: mapSessionToAncestorHolds: list SessionHolds failed; "+
			"this is the SOLE prompt trigger for a newly-delegated child under an active hold; a missed one is picked up only by the manager's ~10h resync, not a later reconcile",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return nil
	}

	// Resolved ONCE: every hold below is tested for membership in this one
	// set instead of re-walking sess's identical ancestor chain from scratch
	// per candidate hold. sess itself is deliberately excluded here (rather
	// than folded into the set) so the "hold names sess itself" case below
	// keeps reading as its own explicit rule.
	ancestors := map[client.ObjectKey]struct{}{}
	if err := spiceboxv1alpha1.WalkAncestors(ctx, r.Client, sess, func(cur *spiceboxv1alpha1.AgentSession) (bool, error) {
		if cur.Namespace == sess.Namespace && cur.Name == sess.Name {
			return false, nil // sess itself is not its own ancestor
		}
		ancestors[client.ObjectKey{Namespace: cur.Namespace, Name: cur.Name}] = struct{}{}
		return false, nil
	}); err != nil {
		logger.Info("sessionhold: mapSessionToAncestorHolds: walk ancestors failed; "+
			"this is the SOLE prompt trigger for a newly-delegated child under an active hold; a missed one is picked up only by the manager's ~10h resync, not a later reconcile",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return nil
	}

	var reqs []reconcile.Request
	for i := range holds.Items {
		h := &holds.Items[i]
		if h.IsReleased() {
			continue
		}
		if h.Spec.SessionRef.Namespace == sess.Namespace && h.Spec.SessionRef.Name == sess.Name {
			// The hold names sess itself, not an ancestor -- cascadeHold only ever
			// fans out to DESCENDANTS, so a hold directly on sess has nothing here
			// to catch; the ordinary SessionHold watch already covers it.
			continue
		}
		if _, isAncestor := ancestors[client.ObjectKey{Namespace: h.Spec.SessionRef.Namespace, Name: h.Spec.SessionRef.Name}]; isAncestor {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: h.Namespace, Name: h.Name}})
		}
	}
	return reqs
}
