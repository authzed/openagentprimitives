package agentsession

import (
	"context"
	"errors"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// RestoreOrCloneBundlePVCs orchestrates workspace PVC restoration for
// the child AgentSession. Behaviour depends on whether any bundles had
// stateful dispatches after the cut turn:
//
//   - IMPACTFUL (len(snapshots) > 0): for each affected bundle, call
//     Snapshotter.Restore with the JIT snapshot handle and the child's
//     workspace PVC as destination. The snapshot was taken (and waited
//     to completion) by the toolcall controller before the first
//     post-cut stateful dispatch.
//
//   - CLEAN (len(snapshots) == 0): no post-cut stateful dispatches
//     occurred, but the parent's workspace still holds user data from
//     turns 0..N. Perform an ad-hoc Snapshot of the parent's current
//     workspace PVC followed by a Restore into the child's workspace
//     PVC so the child starts with identical filesystem state.
//
// Snapshot/Restore only *create* Jobs; the copies complete
// asynchronously. This function therefore sequences on the Done probes:
// the restore Job is never created before the snapshot Job completed
// (otherwise the restore's cp can race the snapshot and copy an
// empty/partial workspace), and it returns pending=true while any Job
// is still running so the caller can requeue and re-enter — every step
// is idempotent (AlreadyExists no-ops, probes are reads). A terminal
// Job failure surfaces as an error wrapping
// workspace.ErrSnapshotJobFailed; nothing completes silently. A
// NotFound from a Done probe is treated as pending, not permanent: the
// corresponding create was issued earlier in the same pass, so the Job
// exists server-side and the probe merely read a lagging informer cache.
//
// A HALF-copied workspace can never be mistaken for a finished one, and
// each path is closed by a different mechanism — state both, because
// neither is visible from this file alone:
//
//   - Both paths: "done" is the Job's JobComplete condition
//     (workspace.CPByPod.jobDone), which means the `set -e; cp -a` container
//     exited 0. A cp that is killed mid-tree exits non-zero, so the Job goes
//     to JobFailed after its backoff and surfaces as ErrSnapshotJobFailed —
//     it can never present as complete.
//   - IMPACTFUL: the "already waited to completion" claim above is ENFORCED,
//     not conventional. Each handle here comes from a tool_dispatch_snapshot
//     memory entry, and toolcall.WaitPreDispatchSnapshot writes that entry
//     only after observing job.Status.Succeeded > 0. A snapshot still running
//     has no audit entry, so AnalyzePostCut cannot produce a handle for it and
//     no restore is created against it.
//   - CLEAN: the ad-hoc snapshot reads the PARENT's live workspace, which is
//     safe only because ReconcileRestart's phase gate admits a parent in
//     Idle/Succeeded/Failed and never Running — no agent turn is writing into
//     the source tree while the cp walks it.
//
// `parent` is used to derive the snapshot-store PVC name (the snapshot
// always lives on the parent's snapshot-store, not the child's).
//
// Today all bundles share a single workspace PVC (per ensureWorkspacePVC).
// When per-bundle PVCs ship this function will need to derive the dst
// PVC from the specific SpiceboxSession.
func RestoreOrCloneBundlePVCs(ctx context.Context, s workspace.Snapshotter, parent, child *spiceboxv1alpha1.AgentSession, snapshots map[string]*workspace.SnapshotHandle) (pending bool, err error) {
	parentWS := workspace.PVCRef{
		Namespace: parent.Namespace,
		Name:      podspec.WorkspaceClaimName(parent),
	}
	childWS := workspace.PVCRef{
		Namespace: child.Namespace,
		Name:      podspec.WorkspaceClaimName(child),
	}
	parentStorePVC := podspec.SnapshotStoreClaimName(parent)

	if len(snapshots) == 0 {
		// CLEAN fork: no post-cut stateful dispatches, but the parent's
		// workspace has accumulated data from prior turns. Snapshot the
		// parent's current state ad-hoc (TurnIndex=-1 as a clean-fork
		// marker) then restore into the child so both start identical.
		adHoc := workspace.SnapshotHandle{
			SessionUID: string(parent.UID),
			TurnIndex:  -1, // ad-hoc clean-fork marker
			Sequence:   0,
			// Qualifier scopes the ad-hoc snapshot to THIS fork: a second
			// fork of the same parent must take a fresh snapshot of the
			// parent's *current* workspace, not silently restore the first
			// fork's stale one. A same-fork replay keeps the same child
			// name → same handle → idempotent.
			Qualifier:        child.Name,
			SnapshotStorePVC: parentStorePVC,
		}
		if err := s.Snapshot(ctx, parentWS, adHoc); err != nil {
			return false, fmt.Errorf("RestoreOrCloneBundlePVCs: snapshot parent: %w", err)
		}
		done, derr := s.SnapshotDone(ctx, parentWS, adHoc)
		if errors.Is(derr, workspace.ErrSnapshotNotFound) {
			// NotFound from the probe is *pending*, not permanent: the
			// Snapshot call above already issued the create in this same
			// pass (success or AlreadyExists), so the Job provably exists
			// server-side — the probe's Get just ran through the
			// operator's informer cache before it caught up (CPByPod is
			// wired with the manager's cached client, while Create writes
			// straight to the apiserver). Classifying this as permanent
			// would let a cache blink silently abort the fork; pending
			// lets the next poll pass converge. The genuinely-missing-
			// snapshot abort is NOT lost by this: absent snapshot data
			// surfaces as the restore Job's cp failing → JobFailed →
			// ErrSnapshotJobFailed → permanent.
			return true, nil
		}
		if derr != nil {
			return false, fmt.Errorf("RestoreOrCloneBundlePVCs: snapshot parent: %w", derr)
		}
		if !done {
			return true, nil
		}
		// Snapshot complete — only now may the restore Job be created.
		if err := s.Restore(ctx, adHoc, childWS); err != nil {
			return false, fmt.Errorf("RestoreOrCloneBundlePVCs: restore ad-hoc: %w", err)
		}
		done, derr = s.RestoreDone(ctx, adHoc, childWS)
		if errors.Is(derr, workspace.ErrSnapshotNotFound) {
			// Same cache-lag rule as the snapshot probe above: Restore
			// just issued the create, so NotFound = pending.
			return true, nil
		}
		if derr != nil {
			return false, fmt.Errorf("RestoreOrCloneBundlePVCs: restore ad-hoc: %w", derr)
		}
		return !done, nil
	}

	// IMPACTFUL: restore each affected bundle from its JIT snapshot. The
	// JIT snapshots were already waited to completion by the toolcall
	// controller pre-dispatch, but the restore Jobs created here still
	// need the same completion+failure discipline: keep launching the
	// remaining restores (they run concurrently), report pending while
	// any is still running.
	for bundle, h := range snapshots {
		if h == nil {
			continue
		}
		handle := *h
		handle.SnapshotStorePVC = parentStorePVC
		if err := s.Restore(ctx, handle, childWS); err != nil {
			return false, fmt.Errorf("RestoreOrCloneBundlePVCs: restore bundle %q: %w", bundle, err)
		}
		done, derr := s.RestoreDone(ctx, handle, childWS)
		if errors.Is(derr, workspace.ErrSnapshotNotFound) {
			// Same cache-lag rule as the CLEAN path: Restore just issued
			// the create in this pass, so a NotFound from the probe is
			// informer-cache lag → pending, never permanent.
			pending = true
			continue
		}
		if derr != nil {
			return false, fmt.Errorf("RestoreOrCloneBundlePVCs: restore bundle %q: %w", bundle, derr)
		}
		if !done {
			pending = true
		}
	}
	return pending, nil
}
