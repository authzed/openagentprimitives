// pkg/controllers/agentsession/storagereclaim.go
//
// reconcileStorageReclaim implements the operator's session-storage retention
// sweep: a TERMINAL (Succeeded/Failed) AgentSession whose retention has
// elapsed past status.finishedAt has its workspace and snapshot-store PVCs
// deleted. The AgentSession itself — transcript, memory records, SpiceDB
// relationships — is deliberately KEPT; only the re-creatable scratch volumes
// go, because they are what accumulates on node disks (the workspace class is
// node-local hostPath with no quota enforcement — a cluster that never
// reclaims them eventually fails every clone with ENOSPC, which is the
// production failure this sweep exists to prevent).
//
// Deleting them is safe at any later wake or restart: both claims are
// ensure-if-missing at provisioning time (ensureWorkspacePVC,
// EnsureSnapshotStorePVC), so a resumed session gets fresh, empty storage and
// re-clones — degraded by one fetch, never wrong.
//
// That re-provisioning is exactly why the StorageReclaimed condition is NOT a
// write-once gate on the sweep. A swept session that wakes re-creates both
// claims and finishes again; a marker that suppressed the sweep for good
// stranded that second lifetime's storage on a node's local disk forever —
// the very accumulation above, reintroduced by the thing meant to prevent it.
// What keeps re-provisioned storage safe from a stale decision is the
// deadline, not the marker: it is measured from the CURRENT
// status.finishedAt, so fresh storage always gets a fresh, full retention
// window, and the call site sweeps only terminal, non-waking sessions. The
// marker's remaining job is narrow — it stops the steady-state no-op pass
// from re-logging and re-writing status on every reconcile of a long-dead
// session.
//
// Scope: the call site (controller.go's terminal block) invokes this only for
// terminal, non-waking sessions; the phase guard here is defense in depth.
// Like reconcileArchive, the sweep runs without the AgentClass gate — a
// terminal session whose class was deleted still reclaims, on the operator
// default.
package agentsession

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

// effectiveStorageRetention is the PURE retention policy: it caps the resolved
// retention (operator default or per-class override) at the SHORT node-pinned
// grace when the workspace class is node-local/local-path
// (cloud.BundledWorkspaceStorageClass) and that grace is set (> 0).
//
// Why the cap: local-path PV bytes live on ONE node's disk with no quota, so a
// terminal session's scratch volumes left for the normal 72h retention pile up
// and can exhaust a node's ephemeral storage. Capping to a few minutes bounds
// that. The cap OVERRIDES both a longer base AND a 'disabled' (<=0) one — disk
// safety wins — but an already-shorter base is honored (min), so an operator
// who wanted an even tighter reclaim still gets it. It never touches a
// cross-node RWX (Filestore/NFS) class: no single node is at risk there.
//
// Trade-off (deliberate): reclaiming node-pinned scratch this aggressively
// gives up restart-from-here / fork for the session on local-path, since both
// re-provision empty and re-clone. That is acceptable for disk safety and is
// why the grace is minutes (a debug window), not seconds.
func effectiveStorageRetention(base time.Duration, nodePinned bool, nodePinnedGrace time.Duration) time.Duration {
	if nodePinned && nodePinnedGrace > 0 && (base <= 0 || nodePinnedGrace < base) {
		return nodePinnedGrace
	}
	return base
}

// reconcileStorageReclaim evaluates the retention deadline and deletes the
// session's scratch claims once past it. Returns:
//   - requeueAfter: time until the deadline (0 = nothing further to schedule).
//   - error: a deletion or status-write failure (the caller logs; the next
//     reconcile retries — both deletes and the condition write are idempotent).
func (r *Reconciler) reconcileStorageReclaim(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (time.Duration, error) {
	if !isTerminalPhase(sess.Status.Phase) {
		return 0, nil
	}
	// Per-class override (> 0) beats the operator default — the same
	// precedence reconcileArchive applies to archiveAfter. The class is
	// fetched tolerantly: a deleted class must not leave a terminal session's
	// storage unreclaimable forever.
	retention := r.DefaultSessionStorageRetention
	var ac spiceboxv1alpha1.AgentClass
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.Class}, &ac); err == nil {
		if ac.Spec.Channels != nil && ac.Spec.Channels.StorageRetention.Duration > 0 {
			retention = ac.Spec.Channels.StorageRetention.Duration
		}
	}
	// Node-local/local-path workspaces are the disk-pressure-prone case: cap
	// their retention at the short node-pinned grace so terminal-session scratch
	// volumes cannot pile onto one node's disk. Cross-node RWX classes keep the
	// long retention (no node is at risk). See effectiveStorageRetention.
	retention = effectiveStorageRetention(retention,
		r.WorkspaceStorageClass == cloud.BundledWorkspaceStorageClass, r.NodePinnedStorageReclaimGrace)
	if retention <= 0 {
		return 0, nil // disabled
	}

	// FinishedAt is stamped on every operator-driven terminal transition — including
	// the SECOND one a woken session reaches — so the deadline tracks the latest
	// life, never the one the marker was written for. The creation-timestamp
	// fallback covers a terminal phase reached without the stamp, so such a session
	// still reclaims instead of parking forever.
	ref := sess.CreationTimestamp.Time
	if sess.Status.FinishedAt != nil {
		ref = sess.Status.FinishedAt.Time
	}
	deadline := ref.Add(retention)
	now := r.now()
	if now.Before(deadline) {
		return deadline.Sub(now), nil
	}

	// Past deadline: delete both claims. NotFound is success: the claim may
	// never have existed (no storage class) or a prior pass got it. What
	// deleteScratchClaims reports is the sweep's state signal — whether anything
	// was actually still there — read straight from the cluster rather than
	// inferred from the marker, so a claim a wake re-provisioned is always seen.
	deleted, err := r.deleteScratchClaims(ctx, sess)
	if err != nil {
		return 0, fmt.Errorf("storage reclaim: %w", err)
	}

	// Steady state: the marker is set and nothing came back. Returning here
	// keeps a long-dead session's reconciles silent instead of re-logging a
	// deletion that deleted nothing and rewriting an unchanged condition.
	if len(deleted) == 0 && conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStorageReclaimed) {
		return 0, nil
	}

	log.FromContext(ctx).Info("storage reclaim: deleted terminal session's scratch claims",
		"session", sess.Namespace+"/"+sess.Name,
		"deleted", deleted,
		"workspace", podspec.WorkspaceClaimName(sess),
		"snapshotStore", podspec.SnapshotStoreClaimName(sess),
		"retention", retention.String())

	patched := sess.DeepCopy()
	conditions.Set(sess, &patched.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionStorageReclaimed, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonAgentSessionStorageReclaimed,
		Message: fmt.Sprintf("workspace and snapshot-store claims deleted %s after the session finished",
			retention),
	})
	if err := agentstatus.WriteOwned(ctx, r.Client, patched, sess, spiceboxv1alpha1.OwnerOperator); err != nil {
		// The claims are gone but the marker is not: the next reconcile
		// re-runs, finds both claims NotFound, and writes the marker then.
		return 0, fmt.Errorf("storage reclaim: record StorageReclaimed: %w", err)
	}
	return 0, nil
}

// deleteScratchClaims deletes the session's workspace and snapshot-store PVCs by
// their derived names — the same helpers provisioning uses, never retyped — and
// reports which were actually still present. NotFound is success (the claim may
// never have existed, or a prior pass got it). Shared by the terminal and idle
// reclaim sweeps so the set of names that count as "scratch" is defined once.
func (r *Reconciler) deleteScratchClaims(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (deleted []string, err error) {
	for _, name := range []string{podspec.WorkspaceClaimName(sess), podspec.SnapshotStoreClaimName(sess)} {
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: sess.Namespace, Name: name}}
		switch derr := r.Client.Delete(ctx, pvc); {
		case derr == nil:
			deleted = append(deleted, name)
		case apierrors.IsNotFound(derr):
			// Already gone — nothing to reclaim for this name.
		default:
			return deleted, fmt.Errorf("delete PVC %s/%s: %w", sess.Namespace, name, derr)
		}
	}
	return deleted, nil
}

// reconcileIdleStorageReclaim frees the scratch volumes of a SLEPT, node-pinned
// Idle session once it has been asleep longer than IdleStorageReclaimAfter,
// keeping the session Idle and wakeable — wake re-provisions both claims empty
// (ensure-if-missing), so the session degrades by one re-clone, never wrong.
//
// Why idle, not just terminal: the idle-sleep reaper frees a parked session's
// CPU but deliberately KEEPS its PVCs, so a re-triggered inbound re-joins with a
// warm clone. On node-local/hostPath storage that scratch is charged to one
// node's disk with no quota, and an unbounded population of parked, wakeable
// sessions accumulates it until the node crosses the ephemeral-storage
// watermark (the disk-pressure this sweep exists to bound). A session asleep
// past the grace gives its warm clone back, at the cost of one re-clone if it
// ever wakes — and its next PV binds wherever it can schedule, so the sweep also
// dissolves the node-affinity wake-deadlock a stranded, node-pinned PV creates.
//
// Gated to the node-pinned class only — the same distinction
// effectiveStorageRetention makes for the terminal sweep — because a cross-node
// RWX class puts no single node at risk. Returns the requeue until the reclaim
// deadline (0 = nothing further to schedule).
func (r *Reconciler) reconcileIdleStorageReclaim(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (time.Duration, error) {
	// A no-op unless the sweep is enabled, the class is node-pinned, and the
	// session is Idle AND slept — the slept gate guarantees the pods are already
	// reaped, so nothing is mid-write when the storage goes.
	if r.IdleStorageReclaimAfter <= 0 ||
		r.WorkspaceStorageClass != cloud.BundledWorkspaceStorageClass ||
		sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseIdle ||
		sess.Status.SleptAt == nil {
		return 0, nil
	}

	deadline := sess.Status.SleptAt.Time.Add(r.IdleStorageReclaimAfter)
	now := r.now()
	if now.Before(deadline) {
		return deadline.Sub(now), nil
	}

	deleted, err := r.deleteScratchClaims(ctx, sess)
	if err != nil {
		return 0, fmt.Errorf("idle storage reclaim: %w", err)
	}

	// Steady state: marker already set and nothing came back. Keep a
	// long-parked session's reconciles silent — no re-log, no status rewrite.
	// The marker never gates the sweep (the deadline off the CURRENT SleptAt
	// does): a woken session re-provisions, sleeps again, and its fresh SleptAt
	// earns a fresh grace, so a stale-True marker can never strand that storage.
	if len(deleted) == 0 && conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStorageReclaimed) {
		return 0, nil
	}

	log.FromContext(ctx).Info("idle storage reclaim: deleted slept session's scratch claims",
		"session", sess.Namespace+"/"+sess.Name,
		"deleted", deleted,
		"workspace", podspec.WorkspaceClaimName(sess),
		"snapshotStore", podspec.SnapshotStoreClaimName(sess),
		"idleReclaimAfter", r.IdleStorageReclaimAfter.String())

	patched := sess.DeepCopy()
	conditions.Set(sess, &patched.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionStorageReclaimed, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonAgentSessionStorageReclaimed,
		Message: fmt.Sprintf("workspace and snapshot-store claims deleted %s after the session went idle; re-provisioned on wake",
			r.IdleStorageReclaimAfter),
	})
	if err := agentstatus.WriteOwned(ctx, r.Client, patched, sess, spiceboxv1alpha1.OwnerOperator); err != nil {
		return 0, fmt.Errorf("idle storage reclaim: record StorageReclaimed: %w", err)
	}
	return 0, nil
}
