package agentsession_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

type snapshotCall struct {
	src workspace.PVCRef
	h   workspace.SnapshotHandle
}

type restoreCall struct {
	h   workspace.SnapshotHandle
	dst workspace.PVCRef
}

type fakeSnapshotter struct {
	snapshotCalls []snapshotCall
	restoreCalls  []restoreCall
	snapshotErr   error
	restoreErr    error

	// Done-probe knobs. The pending flags are zero-value-inverted so a
	// bare &fakeSnapshotter{} behaves like instantly-completing Jobs
	// (pending=false → Done reports true) and every pre-probe test keeps
	// passing unchanged. The err fields, when set, win over the flags.
	snapshotPending bool
	restorePending  bool
	snapshotDoneErr error
	restoreDoneErr  error
}

func (f *fakeSnapshotter) Snapshot(_ context.Context, src workspace.PVCRef, h workspace.SnapshotHandle) error {
	f.snapshotCalls = append(f.snapshotCalls, snapshotCall{src, h})
	return f.snapshotErr
}
func (f *fakeSnapshotter) Restore(_ context.Context, h workspace.SnapshotHandle, dst workspace.PVCRef) error {
	f.restoreCalls = append(f.restoreCalls, restoreCall{h, dst})
	return f.restoreErr
}
func (f *fakeSnapshotter) GC(_ context.Context, _ workspace.SnapshotHandle) error { return nil }
func (f *fakeSnapshotter) SnapshotDone(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) (bool, error) {
	if f.snapshotDoneErr != nil {
		return false, f.snapshotDoneErr
	}
	return !f.snapshotPending, nil
}
func (f *fakeSnapshotter) RestoreDone(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) (bool, error) {
	if f.restoreDoneErr != nil {
		return false, f.restoreDoneErr
	}
	return !f.restorePending, nil
}

func TestRestoreOrCloneBundlePVCs_OneRestorePerAffectedBundle(t *testing.T) {
	ctx := context.Background()
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns"}}
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns"}}
	snaps := map[string]*workspace.SnapshotHandle{
		"b1": {SessionUID: "uid", TurnIndex: 6, Sequence: 0},
		"b2": {SessionUID: "uid", TurnIndex: 7, Sequence: 1},
	}
	snap := &fakeSnapshotter{}

	pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, snaps)
	require.NoError(t, err)
	assert.False(t, pending, "completed restores must not report pending")

	// IMPACTFUL path: no Snapshot calls, one Restore per affected bundle.
	assert.Empty(t, snap.snapshotCalls, "IMPACTFUL path must not call Snapshot")
	require.Len(t, snap.restoreCalls, 2)
	// Each restore should target the child's workspace PVC and have
	// SnapshotStorePVC set from the parent.
	for _, call := range snap.restoreCalls {
		assert.Contains(t, call.dst.Name, "child")
		assert.NotEmpty(t, call.h.SnapshotStorePVC, "handle must carry parent store PVC")
		assert.Contains(t, call.h.SnapshotStorePVC, "parent")
	}
}

func TestRestoreOrCloneBundlePVCs_CleanForkUsesAdHocSnapshot(t *testing.T) {
	ctx := context.Background()
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns", UID: "parent-uid"}}
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns"}}
	snap := &fakeSnapshotter{}

	// CLEAN fork: no post-cut stateful dispatches (nil / empty snapshots).
	pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, nil)
	require.NoError(t, err)
	assert.False(t, pending, "both Jobs done → not pending")

	// Must perform exactly one Snapshot (of the parent's workspace) and
	// one Restore (into the child's workspace).
	require.Len(t, snap.snapshotCalls, 1, "CLEAN path must call Snapshot once")
	require.Len(t, snap.restoreCalls, 1, "CLEAN path must call Restore once")

	sc := snap.snapshotCalls[0]
	assert.Contains(t, sc.src.Name, "parent", "Snapshot source must be parent's workspace PVC")
	assert.Equal(t, "parent-uid", sc.h.SessionUID)
	assert.Equal(t, -1, sc.h.TurnIndex, "TurnIndex=-1 marks ad-hoc clean-fork snapshot")
	assert.Equal(t, 0, sc.h.Sequence)
	assert.Equal(t, "child", sc.h.Qualifier,
		"ad-hoc handle must be qualified by the child name so a second fork gets a fresh snapshot")
	assert.Contains(t, sc.h.SnapshotStorePVC, "parent")

	rc := snap.restoreCalls[0]
	assert.Contains(t, rc.dst.Name, "child", "Restore destination must be child's workspace PVC")
	assert.Equal(t, -1, rc.h.TurnIndex, "Restore handle must reference the ad-hoc snapshot")
	assert.Equal(t, sc.h, rc.h, "Restore must use the exact handle that was snapshotted")
}

func TestRestoreOrCloneBundlePVCs_PropagatesSnapshotterError(t *testing.T) {
	ctx := context.Background()
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns"}}
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns"}}
	snaps := map[string]*workspace.SnapshotHandle{
		"b1": {SessionUID: "uid", TurnIndex: 6},
	}
	boom := errors.New("snapshot store unavailable")
	snap := &fakeSnapshotter{restoreErr: boom}

	pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, snaps)
	require.Error(t, err)
	assert.False(t, pending)
	assert.ErrorIs(t, err, boom)
}

// TestRestoreOrCloneBundlePVCs_CleanFork_WaitsForSnapshotBeforeRestore is the
// core ordering regression test for the CLEAN-fork race: Snapshot/Restore
// return at Job *creation*, so calling Restore right after Snapshot lets the
// restore's cp run against a snapshot that hasn't finished — the child starts
// with an empty/partial workspace. The restore Job must not exist until the
// snapshot Job completed.
func TestRestoreOrCloneBundlePVCs_CleanFork_WaitsForSnapshotBeforeRestore(t *testing.T) {
	ctx := context.Background()
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns", UID: "parent-uid"}}
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns"}}

	// Pass 1: snapshot still running → pending, and crucially NO Restore.
	snap := &fakeSnapshotter{snapshotPending: true}
	pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, nil)
	require.NoError(t, err)
	assert.True(t, pending, "an incomplete snapshot must hold the fork")
	require.Len(t, snap.snapshotCalls, 1)
	assert.Empty(t, snap.restoreCalls, "the restore Job must NOT be created before the snapshot completes")

	// Pass 2 (requeue): snapshot done, restore now created but still
	// running → pending again.
	snap.snapshotPending = false
	snap.restorePending = true
	pending, err = agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, nil)
	require.NoError(t, err)
	assert.True(t, pending, "an incomplete restore must hold the fork")
	require.Len(t, snap.restoreCalls, 1, "restore is created once the snapshot completed")

	// Pass 3 (requeue): restore done → converged.
	snap.restorePending = false
	pending, err = agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, nil)
	require.NoError(t, err)
	assert.False(t, pending, "both Jobs complete → the fork may proceed")
}

// TestRestoreOrCloneBundlePVCs_ImpactfulRestorePending mirrors the wait
// discipline on the IMPACTFUL path: the JIT snapshots were completed
// pre-dispatch, but the restore Jobs still complete asynchronously.
func TestRestoreOrCloneBundlePVCs_ImpactfulRestorePending(t *testing.T) {
	ctx := context.Background()
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns"}}
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns"}}
	snaps := map[string]*workspace.SnapshotHandle{
		"b1": {SessionUID: "uid", TurnIndex: 6, Sequence: 0},
		"b2": {SessionUID: "uid", TurnIndex: 7, Sequence: 1},
	}

	snap := &fakeSnapshotter{restorePending: true}
	pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, snaps)
	require.NoError(t, err)
	assert.True(t, pending, "running restore Jobs must report pending")
	// All restores are still launched (they run concurrently); pending is
	// reported after the sweep, not on the first running Job.
	assert.Len(t, snap.restoreCalls, 2)

	snap.restorePending = false
	pending, err = agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, snaps)
	require.NoError(t, err)
	assert.False(t, pending)
}

// TestRestoreOrCloneBundlePVCs_ProbeNotFound_IsPending pins the cache-lag
// rule: CPByPod is wired with the manager's CACHED client, so a Done probe
// issued microseconds after the same pass's create can read NotFound from a
// lagging informer even though the Job exists server-side. That NotFound
// must classify as pending (requeue and converge) — treating it as permanent
// would let a cache blink silently abort every first-pass CLEAN fork.
func TestRestoreOrCloneBundlePVCs_ProbeNotFound_IsPending(t *testing.T) {
	ctx := context.Background()
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns", UID: "parent-uid"}}
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns"}}
	notFound := fmt.Errorf("CPByPod: Job ns/snap-x: %w", workspace.ErrSnapshotNotFound)

	t.Run("CLEAN snapshot probe NotFound → pending, no restore, no error", func(t *testing.T) {
		snap := &fakeSnapshotter{snapshotDoneErr: notFound}
		pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, nil)
		require.NoError(t, err, "cache-lag NotFound must not surface as an error")
		assert.True(t, pending)
		assert.Empty(t, snap.restoreCalls, "no restore before the snapshot is observably complete")
	})

	t.Run("CLEAN restore probe NotFound → pending, no error", func(t *testing.T) {
		snap := &fakeSnapshotter{restoreDoneErr: notFound}
		pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, nil)
		require.NoError(t, err)
		assert.True(t, pending)
		require.Len(t, snap.restoreCalls, 1, "the restore create was issued in this pass")
	})

	t.Run("IMPACTFUL restore probe NotFound → pending, no error", func(t *testing.T) {
		snap := &fakeSnapshotter{restoreDoneErr: notFound}
		snaps := map[string]*workspace.SnapshotHandle{"b1": {SessionUID: "uid", TurnIndex: 6}}
		pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, snaps)
		require.NoError(t, err)
		assert.True(t, pending)
	})
}

// TestRestoreOrCloneBundlePVCs_DoneProbeFailures pins the failure surface:
// a terminally-failed snapshot or restore Job must propagate as an error
// (wrapping workspace.ErrSnapshotJobFailed) — never as a silent success or
// an eternal pending.
func TestRestoreOrCloneBundlePVCs_DoneProbeFailures(t *testing.T) {
	ctx := context.Background()
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "ns", UID: "parent-uid"}}
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns"}}

	t.Run("failed snapshot Job on CLEAN path → ErrSnapshotJobFailed, no restore", func(t *testing.T) {
		snap := &fakeSnapshotter{
			snapshotDoneErr: fmt.Errorf("job ns/snap-x failed: backoff limit exceeded: %w", workspace.ErrSnapshotJobFailed),
		}
		pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, nil)
		require.Error(t, err)
		assert.False(t, pending)
		assert.ErrorIs(t, err, workspace.ErrSnapshotJobFailed)
		assert.Empty(t, snap.restoreCalls, "a failed snapshot must not spawn a restore")
	})

	t.Run("failed restore Job on CLEAN path → ErrSnapshotJobFailed", func(t *testing.T) {
		snap := &fakeSnapshotter{
			restoreDoneErr: fmt.Errorf("job ns/rest-x failed: backoff limit exceeded: %w", workspace.ErrSnapshotJobFailed),
		}
		pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, nil)
		require.Error(t, err)
		assert.False(t, pending)
		assert.ErrorIs(t, err, workspace.ErrSnapshotJobFailed)
	})

	t.Run("failed restore Job on IMPACTFUL path → ErrSnapshotJobFailed", func(t *testing.T) {
		snap := &fakeSnapshotter{
			restoreDoneErr: fmt.Errorf("job ns/rest-x failed: %w", workspace.ErrSnapshotJobFailed),
		}
		snaps := map[string]*workspace.SnapshotHandle{"b1": {SessionUID: "uid", TurnIndex: 6}}
		pending, err := agentsession.RestoreOrCloneBundlePVCs(ctx, snap, parent, child, snaps)
		require.Error(t, err)
		assert.False(t, pending)
		assert.ErrorIs(t, err, workspace.ErrSnapshotJobFailed)
	})
}
