// Package agentstatus writes AgentSession status the way a multi-writer CRD
// requires: each writer (the operator, channelsd) touches ONLY the fields it
// changed, so it never reverts a stale field or clears a field a concurrent
// writer owns. WriteOwned sends only the diff — scalars/queues via a
// resourceVersion-free merge patch (an unincluded field is left untouched), and
// conditions, where a merge patch would replace the co-owned array wholesale, via
// a fresh reload + Update under conflict retry.
package agentstatus

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// WriteOwned persists the changes a writer made to sess since `original` (the
// object as read at the start of its reconcile), touching only the fields it
// changed.
//
//   - Non-condition fields (phase, pending queues, other scalars) go through a
//     resourceVersion-free JSON merge patch of just the diff. A field the
//     reconcile did not change is never re-sent, so the operator can't flap
//     phase on a stale read and neither writer reverts the other's scalars; being
//     resourceVersion-free, it never conflicts.
//   - Condition changes (for the writer's owned types) go through a fresh reload +
//     Set/Remove + a conditions-only merge patch (optimistic-locked) under
//     RetryOnConflict. Reloading fresh means the co-owned conditions array already
//     holds the other writer's entries; the merge patch replaces that
//     now-complete array without re-serializing any other status field. A full
//     Update was used originally, but it re-serialized the ENTIRE status —
//     including operator-owned fields like effectiveSettings — so a writer whose
//     compiled types lagged the CRD by a required field (e.g.
//     effectiveSettings.reportSessionCost) silently dropped it on the round-trip
//     and the apiserver rejected the whole write. Patching only the conditions
//     diff keeps a condition write robust to that skew.
//
// owner selects the writer's condition types (operator = non-approval,
// channelsd = approval).
//
// CALLER CONTRACT: `original` must be what THIS writer last persisted (or, for a
// first write, the object as just read) — never an older snapshot it has already
// written past. The non-condition merge patch is resourceVersion-free, so any
// field present in it wins unconditionally; re-sending a field the caller
// already persisted therefore reverts whatever a concurrent writer put there in
// the meantime. A caller that writes more than once must advance its base after
// each successful write (see agentsession.reconcileState).
func WriteOwned(ctx context.Context, c client.Client, sess, original *spiceboxv1alpha1.AgentSession, owner spiceboxv1alpha1.StatusFieldOwner) error {
	// Non-condition fields: merge patch of only the diff. Neutralize conditions in
	// the patch base so they never appear in this patch — they are handled below.
	base := original.DeepCopy()
	base.Status.Conditions = sess.Status.Conditions
	if !equality.Semantic.DeepEqual(base.Status, sess.Status) {
		if err := c.Status().Patch(ctx, sess.DeepCopy(), client.MergeFrom(base)); err != nil {
			return fmt.Errorf("merge-patch AgentSession status %s/%s: %w", sess.Namespace, sess.Name, err)
		}
	}

	// Owned condition changes: fresh reload + conditions-only patch so a co-owned
	// array is never clobbered by a stale snapshot, and no other status field is
	// touched.
	set, removed := diffOwnedConditions(original, sess, owner)
	if len(set) == 0 && len(removed) == 0 {
		return nil
	}
	key := client.ObjectKeyFromObject(sess)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh spiceboxv1alpha1.AgentSession
		if err := c.Get(ctx, key, &fresh); err != nil {
			return fmt.Errorf("reload AgentSession %s/%s for condition write: %w", sess.Namespace, sess.Name, err)
		}
		prior := fresh.DeepCopy()
		for i := range set {
			meta.SetStatusCondition(&fresh.Status.Conditions, set[i])
		}
		for _, t := range removed {
			meta.RemoveStatusCondition(&fresh.Status.Conditions, t)
		}
		// Patch only the conditions diff, carrying the reloaded resourceVersion
		// (optimistic lock) so a concurrent writer's change 409s and RetryOnConflict
		// re-reads. Crucially, the patch body never includes any other status field,
		// so operator-owned fields (effectiveSettings, progress, …) are neither
		// re-serialized nor re-validated against this writer's possibly-stale types.
		return c.Status().Patch(ctx, &fresh, client.MergeFromWithOptions(prior, client.MergeFromWithOptimisticLock{}))
	})
}

// diffOwnedConditions returns the owner's conditions that were added or changed
// (set) and those removed, between original and sess.
func diffOwnedConditions(original, sess *spiceboxv1alpha1.AgentSession, owner spiceboxv1alpha1.StatusFieldOwner) (set []metav1.Condition, removed []string) {
	owns := func(t string) bool {
		return spiceboxv1alpha1.IsApprovalCondition(t) == (owner == spiceboxv1alpha1.OwnerApprovals)
	}
	for i := range sess.Status.Conditions {
		cur := sess.Status.Conditions[i]
		if !owns(cur.Type) {
			continue
		}
		old := meta.FindStatusCondition(original.Status.Conditions, cur.Type)
		if old == nil || old.Status != cur.Status || old.Reason != cur.Reason ||
			old.Message != cur.Message || old.ObservedGeneration != cur.ObservedGeneration {
			set = append(set, cur)
		}
	}
	for i := range original.Status.Conditions {
		old := original.Status.Conditions[i]
		if owns(old.Type) && meta.FindStatusCondition(sess.Status.Conditions, old.Type) == nil {
			removed = append(removed, old.Type)
		}
	}
	return set, removed
}
