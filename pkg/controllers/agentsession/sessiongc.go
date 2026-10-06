// pkg/controllers/agentsession/sessiongc.go
//
// reconcileSessionGC implements the operator's session-lifetime GC: a TERMINAL
// (Succeeded/Failed) AgentSession whose SessionGCAfter window has elapsed past
// status.finishedAt has its whole CR deleted. This is the retention the pod
// reap anticipates — see controller.go's terminal block, "until retention GCs
// the AgentSession" — and the reason downstream consumers already tolerate a
// GC'd session (admind stamps such a session "Gone").
//
// Deleting the CR is safe for the tamper-evident audit log. The append-only
// kinds (transcript, audit, authz-decision, tool-session) live in the durable
// memory backend, NOT on the CR, and finalize never deletes them
// (memory.Local.DeleteScope structurally refuses append-only kinds). The CR
// carries only the K8s-witnessed trust root (status.auditPublicKey/auditKeyID),
// which witnessAuditKey has already mirrored into the session's own durable
// memory scope precisely so records still verify after a delete-and-recreate.
// What a GC'd CR does give up is the truncation anchor (status.auditChainHeads);
// that is a verification aid, and losing it on long-dead sessions is the
// accepted cost of bounding unbounded CR growth.
//
// The bulk of what this reclaims is the owned ToolCall population: ToolCalls
// carry an AgentSession owner-ref with blockOwnerDeletion, so a session that is
// never deleted strands every ToolCall it ever spawned in etcd and in the
// operator's informer cache. Deleting the session cascade-collects them.
package agentsession

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// reconcileSessionGC evaluates the session-lifetime deadline and deletes the
// whole AgentSession CR once past it. Returns:
//   - requeueAfter: time until the GC deadline (0 = nothing further to
//     schedule, e.g. disabled, non-terminal, or already deleted).
//   - deleted: true when the session has been deleted (or was already
//     deleting) and the caller should stop reconciling it this pass.
//   - error: a delete failure (the caller logs; the next reconcile retries —
//     the delete is idempotent).
func (r *Reconciler) reconcileSessionGC(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (time.Duration, bool, error) {
	if r.SessionGCAfter <= 0 {
		return 0, false, nil // disabled
	}
	if !isTerminalPhase(sess.Status.Phase) {
		return 0, false, nil
	}
	// Already deleting (finalize in flight): nothing to schedule, and the caller
	// should stop reconciling this pass.
	if !sess.DeletionTimestamp.IsZero() {
		return 0, true, nil
	}
	// Deadline from finishedAt, with creation as the fallback — the same
	// reference reconcileStorageReclaim uses, so a terminal session that reached
	// its phase without a finishedAt stamp is still collected instead of parking
	// forever.
	ref := sess.CreationTimestamp.Time
	if sess.Status.FinishedAt != nil {
		ref = sess.Status.FinishedAt.Time
	}
	deadline := ref.Add(r.SessionGCAfter)
	now := r.now()
	if now.Before(deadline) {
		return deadline.Sub(now), false, nil
	}

	// Past deadline: delete the whole CR. The resulting deletionTimestamp routes
	// the next reconcile to finalize, which revokes the memory token and SpiceDB
	// grants and KEEPS the append-only audit log; owner-ref GC cascade-collects
	// the ToolCalls, pods and Secrets the session owns. NotFound is success — a
	// prior pass (or another actor) already deleted it.
	if err := r.Client.Delete(ctx, sess); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, true, nil
		}
		return 0, false, fmt.Errorf("session GC: delete terminal session %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	log.FromContext(ctx).Info("session GC: deleted terminal session past retention",
		"session", sess.Namespace+"/"+sess.Name,
		"phase", sess.Status.Phase,
		"gcAfter", r.SessionGCAfter.String())
	return 0, true, nil
}
