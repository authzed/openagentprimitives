package agentsession

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
)

// reconcileStateKey carries the per-reconcile status-write bookkeeping so
// applyStatus can write only the fields it has not already written, without
// threading two snapshots through every call site.
type reconcileStateKey struct{}

// reconcileState holds the two baselines a reconcile needs. They are NOT
// interchangeable:
//
//   - original is the session exactly as read at the start of the reconcile. It
//     answers "was this already true when I started?" — edge-triggering
//     (conditionBecameTrue, the AwaitingCredentials entry check). It never
//     changes for the life of the reconcile.
//   - lastWritten is the state applyStatus most recently persisted. It answers
//     "what have I already sent?" — the diff base for the next write.
//
// Conflating them is a correctness bug, not a nicety. A reconcile calls
// applyStatus several times in one pass (settings snapshot, identity gate, and
// the post-RunnerFactory.Start runner-creating write are three), and diffing
// every one of them against the reconcile-START snapshot re-sends fields the
// FIRST call already persisted. Because agentstatus.WriteOwned sends
// non-condition fields as a resourceVersion-free merge patch — deliberately, so
// it never conflicts — such a re-send silently overwrites whatever a concurrent
// writer put there in between.
//
// That is exactly how a refusal-parked session lost its phase: the reconcile set
// Phase ""→Pending and persisted it, then started the runner; the runner hit a
// provider refusal and patched Phase=AwaitingRetry; the reconcile's next
// applyStatus — still diffing against the phase-"" snapshot — re-sent
// Phase=Pending over it. The runner had already exited, so nothing moved the
// phase again: the session sat in Pending and the user never got the Retry
// button. See TestApplyStatus_SecondWriteDoesNotRevertAConcurrentWriter.
type reconcileState struct {
	original    *spiceboxv1alpha1.AgentSession
	lastWritten *spiceboxv1alpha1.AgentSession
}

// withReconcileOriginal returns a context carrying original. Reconcile sets it
// once, immediately after reading the session and before any status mutation.
func withReconcileOriginal(ctx context.Context, original *spiceboxv1alpha1.AgentSession) context.Context {
	return context.WithValue(ctx, reconcileStateKey{}, &reconcileState{
		original:    original,
		lastWritten: original.DeepCopy(),
	})
}

// reconcileOriginal returns the start-of-reconcile snapshot, or nil when the
// caller did not install one. Use it for edge detection only — the status-write
// diff base is reconcileState.lastWritten (see applyStatus).
func reconcileOriginal(ctx context.Context) *spiceboxv1alpha1.AgentSession {
	st, _ := ctx.Value(reconcileStateKey{}).(*reconcileState)
	if st == nil {
		return nil
	}
	return st.original
}

// applyStatus persists the operator's changes to sess, touching only the fields
// that changed since this reconcile's LAST status write (see agentstatus.WriteOwned
// and the reconcileState godoc). A direct call without the per-reconcile state in
// ctx (a test, or a helper invoked outside Reconcile) falls back to the current
// server state as the base and keeps no bookkeeping.
func (r *Reconciler) applyStatus(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	st, _ := ctx.Value(reconcileStateKey{}).(*reconcileState)
	if st == nil {
		var base spiceboxv1alpha1.AgentSession
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(sess), &base); err != nil {
			return fmt.Errorf("read base for status write %s/%s: %w", sess.Namespace, sess.Name, err)
		}
		return agentstatus.WriteOwned(ctx, r.Client, sess, &base, spiceboxv1alpha1.OwnerOperator)
	}
	if err := agentstatus.WriteOwned(ctx, r.Client, sess, st.lastWritten, spiceboxv1alpha1.OwnerOperator); err != nil {
		return err
	}
	// Advance the diff base only on success: a failed write persisted nothing, so
	// the next attempt must still carry the same diff.
	st.lastWritten = sess.DeepCopy()
	return nil
}
