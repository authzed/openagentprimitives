package agentsession

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// fakeHoldSnapshotter is a package-local test double for workspace.Snapshotter,
// scoped to the reconcileHold sequencing tests below. It is distinct from
// restart_pvc_test.go's fakeSnapshotter, which lives in package
// agentsession_test and is not visible from this (package agentsession) file.
type fakeHoldSnapshotter struct {
	snapshotCalls int
	snapshotErr   error
	pending       bool
	doneErr       error
}

func (f *fakeHoldSnapshotter) Snapshot(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) error {
	f.snapshotCalls++
	return f.snapshotErr
}
func (f *fakeHoldSnapshotter) Restore(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) error {
	return nil
}
func (f *fakeHoldSnapshotter) GC(_ context.Context, _ workspace.SnapshotHandle) error { return nil }
func (f *fakeHoldSnapshotter) SnapshotDone(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) (bool, error) {
	if f.doneErr != nil {
		return false, f.doneErr
	}
	return !f.pending, nil
}
func (f *fakeHoldSnapshotter) RestoreDone(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) (bool, error) {
	return true, nil
}

// TestReconcileHold_activeHoldParksAndStops proves the park half: an
// unreleased SessionHold naming this session must force phase=Held and
// short-circuit (proceed=false) so the caller never reaches derivePhase,
// which would otherwise overwrite it back to whatever the folded lifecycle
// log projects on this exact same reconcile.
func TestReconcileHold_activeHoldParksAndStops(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
			Source:     "tripper/plangate-denial-streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
	}
	_, r := newParkTestReconciler(t, sess, hold)

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed, "an active hold must stop the reconcile before derivePhase")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseHeld, sess.Status.Phase)
}

// TestReconcileHold_fastPathPublishFails_stillReachesHeld is the
// discriminating test for the design's central claim: the ap.revocation fast
// path is best-effort, and Held must be reached even when a publish is
// actually ATTEMPTED and FAILS, not merely when no publisher is wired at all.
// r.RevokePublisher here is wired to fakeBus{fail: true} (shared with
// passthrough_credhash_test.go), so Emit reaches the bus, the bus records the
// call, and returns an error -- reconcileHold's fast-path branch (hold.go)
// must log that error and carry on rather than returning it, so a failed
// publish never blocks the pod-reap containment guarantee that follows it.
func TestReconcileHold_fastPathPublishFails_stillReachesHeld(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
			Source:     "tripper/plangate-denial-streak",
		},
	}
	_, r := newParkTestReconciler(t, sess, hold)
	bus := &fakeBus{fail: true}
	r.RevokePublisher = revocation.NewPublisher(bus)

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err, "a failed fast-path publish must be logged, not returned as a reconcile error")
	assert.False(t, proceed, "an active hold must still stop the reconcile before derivePhase")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseHeld, sess.Status.Phase,
		"Held must be reached even though the fast-path publish was attempted and failed")
	assert.Equal(t, 1, bus.published,
		"the fast-path publish must actually have been attempted against a wired bus, not skipped")
}

// TestReconcileHold_releasedHoldProceeds proves the converse: a SessionHold
// that has already been released (a human cleared the card) must not keep
// parking the session -- the rest of Reconcile, including derivePhase, needs
// to run so the session can leave Held. This is the discriminating test for
// Blocker 1: before reconcileHoldRelease existed, reconcileHold returned
// proceed=true here WITHOUT ever emitting lifecyclecore.Released, so
// sess.Status.Phase stayed exactly "Held" (nothing else in Reconcile moves it
// off) and the durable log never recorded a release at all -- this test fails
// on both counts against that code.
func TestReconcileHold_releasedHoldProceeds(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseHeld},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{
			Phase:      spiceboxv1alpha1.SessionHoldPhaseReleased,
			ReleasedBy: "user:approver-1",
		},
	}
	_, r := newParkTestReconciler(t, sess, hold)
	r.LifecycleMemory = memory.NewLocal(inmem.NewBackend())

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.True(t, proceed, "a released hold must not keep parking the session")
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseHeld, sess.Status.Phase,
		"the session must actually leave Held once its hold is released, not just report proceed=true")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, sess.Status.Phase,
		"Released projects Pending, the same unpark target CredsLinked/WakeRequested use")

	events, err := lifecyclekind.Events(ctx, r.LifecycleMemory, lifecycleScope(sess))
	require.NoError(t, err)
	require.Len(t, events, 1, "the release must be durably recorded in the signed lifecycle log")
	released, ok := events[0].(lifecyclecore.Released)
	require.True(t, ok, "the recorded event must be Released, got %T", events[0])
	assert.Equal(t, "user:approver-1", released.ApprovedBy,
		"the release is attributed to the human who cleared the card, from the hold's ReleasedBy")
}

// TestReconcileHold_noHoldProceeds proves reconcileHold is a no-op for a
// session no SessionHold names -- it must never invent a park.
func TestReconcileHold_noHoldProceeds(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
	}
	_, r := newParkTestReconciler(t, sess)

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.True(t, proceed)
}

// TestReconcileHold_heldPhaseNoHoldAtAllIsUntouched proves the release-resume
// path never invents an attribution: a session stuck at PhaseHeld with NO
// SessionHold CR naming it at all (released or otherwise -- e.g. one force-
// deleted out from under it) has nothing for reconcileHoldRelease to
// attribute a Released event to, so it must leave the session exactly as it
// found it rather than emitting a Released with an empty ApprovedBy.
func TestReconcileHold_heldPhaseNoHoldAtAllIsUntouched(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseHeld},
	}
	_, r := newParkTestReconciler(t, sess) // no SessionHold seeded at all
	r.LifecycleMemory = memory.NewLocal(inmem.NewBackend())

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.True(t, proceed, "no hold to park behind, so the rest of Reconcile must still run")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseHeld, sess.Status.Phase,
		"with no hold at all to attribute a release to, the phase must be left exactly as found")

	events, err := lifecyclekind.Events(ctx, r.LifecycleMemory, lifecycleScope(sess))
	require.NoError(t, err)
	assert.Empty(t, events, "no Released event may be invented with no hold to attribute it to")
}

// TestReconcileHold_activeHoldReapsSessionPods proves containment is real,
// not just a status label: once an active hold's phase override is durably
// recorded, reconcileHold tears down the session's bundle SpiceboxSession and
// runner pod via reapSessionPods, so the agent's compute actually stops.
func TestReconcileHold_activeHoldReapsSessionPods(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseRunning,
			BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
				{Name: "git", SpiceboxSessionName: "s1-git"},
			},
		},
	}
	bundle := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1-git", Namespace: "demo"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "toolbelt"},
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
	}
	c, r := newParkTestReconciler(t, sess, bundle, runner, hold)

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)

	var gotBundle spiceboxv1alpha1.SpiceboxSession
	err = c.Get(ctx, client.ObjectKeyFromObject(bundle), &gotBundle)
	assert.True(t, apierrors.IsNotFound(err), "the bundle SpiceboxSession must be reaped, got err=%v", err)

	var gotRunner corev1.Pod
	err = c.Get(ctx, client.ObjectKeyFromObject(runner), &gotRunner)
	assert.True(t, apierrors.IsNotFound(err), "the runner pod must be reaped, got err=%v", err)
}

// TestReconcileHold_releasedHoldDoesNotReapSessionPods proves the converse:
// a released hold must not touch the session's pods -- only an ACTIVE hold is
// containment, and reconcileHold falls through before reaching the reap step
// once the hold that parked this session has been cleared.
func TestReconcileHold_releasedHoldDoesNotReapSessionPods(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseHeld,
			BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
				{Name: "git", SpiceboxSessionName: "s1-git"},
			},
		},
	}
	bundle := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1-git", Namespace: "demo"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "toolbelt"},
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseReleased},
	}
	c, r := newParkTestReconciler(t, sess, bundle, runner, hold)

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.True(t, proceed)

	var gotBundle spiceboxv1alpha1.SpiceboxSession
	assert.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(bundle), &gotBundle),
		"a released hold must not reap the bundle SpiceboxSession")

	var gotRunner corev1.Pod
	assert.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(runner), &gotRunner),
		"a released hold must not reap the runner pod")
}

// TestMapSessionHoldToSession pins "the watch actually enqueuing": a
// SessionHold change must re-enqueue exactly the AgentSession its
// spec.sessionRef names.
func TestMapSessionHoldToSession(t *testing.T) {
	hold := &spiceboxv1alpha1.SessionHold{
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
		},
	}
	reqs := mapSessionHoldToSession(context.Background(), hold)
	require.Len(t, reqs, 1)
	assert.Equal(t, "s1", reqs[0].Name)
	assert.Equal(t, "demo", reqs[0].Namespace)
}

// TestMapSessionHoldToSession_WrongObjectTypeEnqueuesNothing mirrors
// TestMapCredentialUpdateRequestToSession_WrongObjectTypeEnqueuesNothing: a
// mapper wired via handler.EnqueueRequestsFromMapFunc must fail closed to
// "nothing to enqueue" when handed an object of the wrong type, rather than
// panicking on a bad type assertion.
func TestMapSessionHoldToSession_WrongObjectTypeEnqueuesNothing(t *testing.T) {
	got := mapSessionHoldToSession(context.Background(), &spiceboxv1alpha1.AgentSession{})
	assert.Nil(t, got)
}

// TestHoldSnapshotHandle_isQualifiedByHoldName proves the handle's Qualifier
// is keyed on the hold's name, which is what makes a snapshot idempotent
// across reconciler retries: the same hold always resolves to the same
// handle.
func TestHoldSnapshotHandle_isQualifiedByHoldName(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo", UID: "uid-1"},
	}
	hold := &spiceboxv1alpha1.SessionHold{ObjectMeta: metav1.ObjectMeta{Name: "h1"}}
	r := &Reconciler{}

	h := r.holdSnapshotHandle(sess, hold)
	assert.Equal(t, "uid-1", h.SessionUID)
	assert.Equal(t, "hold-h1", h.Qualifier,
		"keying the qualifier on the hold name makes the snapshot idempotent across reconciler retries")
}

// TestHoldSnapshotHandle_isStableAcrossCalls proves the handle is a pure
// function of (sess, hold): a second reconcile must resolve to the identical
// handle, or a retry would launch a second Job instead of finding the first.
func TestHoldSnapshotHandle_isStableAcrossCalls(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo", UID: "uid-1"},
	}
	hold := &spiceboxv1alpha1.SessionHold{ObjectMeta: metav1.ObjectMeta{Name: "h1"}}
	r := &Reconciler{}

	// Same hold, same handle: a second reconcile must not launch a second Job.
	assert.Equal(t, r.holdSnapshotHandle(sess, hold), r.holdSnapshotHandle(sess, hold))
}

// TestHoldSnapshotHandle_rendersAdHocJobName pins the exact Job name a hold
// snapshot renders to. holdSnapshotHandle sets TurnIndex=-1 (the ad-hoc
// marker workspace.SnapshotHandle.turnSegment branches on, same as
// restart_pvc.go's clean-fork snapshot) because a hold snapshot is not taken
// at a turn boundary. This assertion is what catches a future regression
// (e.g. TurnIndex silently reverting to its zero value) instead of letting a
// seeded test fixture quietly disagree with what the code actually renders.
func TestHoldSnapshotHandle_rendersAdHocJobName(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo", UID: "uid-1"},
	}
	hold := &spiceboxv1alpha1.SessionHold{ObjectMeta: metav1.ObjectMeta{Name: "h1"}}
	r := &Reconciler{}

	h := r.holdSnapshotHandle(sess, hold)
	assert.Equal(t, -1, h.TurnIndex, "a hold snapshot is ad-hoc, not taken at a turn boundary")
	assert.Equal(t, "snap-uid-1-adhoc-hold-h1-000", workspace.SnapshotJobName(h),
		"pins the rendered Job name so a TurnIndex/Qualifier regression is caught here, not silently accepted by a seeded fixture elsewhere")
}

// TestReconcileHold_snapshotCompletes_reapsAndRecordsHandle proves the
// success path end to end: once the workspace snapshot Job reports complete,
// reconcileHold records the handle on the hold's status AND reaps the
// session's pods -- the evidence lands before the compute is torn down.
func TestReconcileHold_snapshotCompletes_reapsAndRecordsHandle(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo", UID: "uid-1"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseRunning,
			BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
				{Name: "git", SpiceboxSessionName: "s1-git"},
			},
		},
	}
	bundle := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1-git", Namespace: "demo"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "toolbelt"},
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
	}
	c, r := newParkTestReconciler(t, sess, bundle, runner, hold)
	snap := &fakeHoldSnapshotter{}
	r.Snapshotter = snap

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)
	assert.Equal(t, 1, snap.snapshotCalls, "exactly one snapshot Job must be launched")

	var persistedHold spiceboxv1alpha1.SessionHold
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(hold), &persistedHold))
	assert.NotEmpty(t, persistedHold.Status.SnapshotHandle,
		"a completed snapshot must be recorded on the hold's status")

	var gotRunner corev1.Pod
	err = c.Get(ctx, client.ObjectKeyFromObject(runner), &gotRunner)
	assert.True(t, apierrors.IsNotFound(err), "the runner pod must be reaped once the snapshot has landed")

	var gotBundle spiceboxv1alpha1.SpiceboxSession
	err = c.Get(ctx, client.ObjectKeyFromObject(bundle), &gotBundle)
	assert.True(t, apierrors.IsNotFound(err), "the bundle SpiceboxSession must be reaped once the snapshot has landed")
}

// TestReconcileHold_snapshotPending_requeuesWithoutReaping proves the
// ordering guarantee: while the snapshot Job is still running, reconcileHold
// must requeue rather than reap -- reaping now would destroy the evidence
// the snapshot exists to preserve.
func TestReconcileHold_snapshotPending_requeuesWithoutReaping(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo", UID: "uid-1"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
	}
	c, r := newParkTestReconciler(t, sess, runner, hold)
	snap := &fakeHoldSnapshotter{pending: true}
	r.Snapshotter = snap

	res, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)
	assert.Positive(t, res.RequeueAfter, "a still-running snapshot must requeue rather than fall through to reap")

	var gotRunner corev1.Pod
	assert.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(runner), &gotRunner),
		"the runner pod must NOT be reaped while the snapshot is still running")

	var persistedHold spiceboxv1alpha1.SessionHold
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(hold), &persistedHold))
	assert.Empty(t, persistedHold.Status.SnapshotHandle, "an incomplete snapshot must not be recorded as done")
}

// TestReconcileHold_snapshotFails_stillReapsAndRecordsFailure proves the
// failure policy: containment beats evidence, so a snapshot launch failure
// must still park and reap the session -- but the failure must be recorded
// loudly on the hold's status rather than proceeding as if a snapshot had
// been taken.
func TestReconcileHold_snapshotFails_stillReapsAndRecordsFailure(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo", UID: "uid-1"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
	}
	c, r := newParkTestReconciler(t, sess, runner, hold)
	snap := &fakeHoldSnapshotter{snapshotErr: errors.New("snapshot store unavailable")}
	r.Snapshotter = snap

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err, "containment must win: a snapshot failure is not itself a reconcile error")
	assert.False(t, proceed)

	var gotRunner corev1.Pod
	err = c.Get(ctx, client.ObjectKeyFromObject(runner), &gotRunner)
	assert.True(t, apierrors.IsNotFound(err), "the runner pod must still be reaped when the snapshot fails")

	var persistedHold spiceboxv1alpha1.SessionHold
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(hold), &persistedHold))
	assert.Contains(t, persistedHold.Status.Containment, "snapshot",
		"the failure must be recorded loudly on the hold's status, not proceeded through silently")
	assert.Empty(t, persistedHold.Status.SnapshotHandle, "a failed snapshot must not be recorded as if it succeeded")
}

// TestReconcileHold_nilSnapshotter_treatedAsFailure_stillReaps proves the nil
// guard: an operator with no Snapshotter configured must not panic, and must
// follow the same containment-wins failure policy as a launch error.
func TestReconcileHold_nilSnapshotter_treatedAsFailure_stillReaps(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo", UID: "uid-1"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
	}
	// newParkTestReconciler leaves Snapshotter nil: this proves the pre-existing
	// containment behavior survives even when the operator has no Snapshotter
	// wired at all.
	c, r := newParkTestReconciler(t, sess, runner, hold)

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)

	var gotRunner corev1.Pod
	err = c.Get(ctx, client.ObjectKeyFromObject(runner), &gotRunner)
	assert.True(t, apierrors.IsNotFound(err), "reap must proceed even when no Snapshotter is configured")

	var persistedHold spiceboxv1alpha1.SessionHold
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(hold), &persistedHold))
	assert.Contains(t, persistedHold.Status.Containment, "snapshot",
		"a missing Snapshotter must be recorded on status, not silent")
}

// TestReconcileHold_secondReconcileWithHandleSet_doesNotRelaunchSnapshot
// proves the hold's status stamp stays idempotent for the snapshot too: once
// hold.Status.SnapshotHandle is recorded, a later reconcile must not launch a
// second snapshot Job for the same hold.
func TestReconcileHold_secondReconcileWithHandleSet_doesNotRelaunchSnapshot(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo", UID: "uid-1"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseHeld},
	}
	trippedAt := metav1.NewTime(time.Now())
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{
			Phase:     spiceboxv1alpha1.SessionHoldPhaseActive,
			TrippedAt: &trippedAt,
		},
	}
	// Derived from the same code path production uses (holdSnapshotHandle +
	// workspace.SnapshotJobName), not hand-typed, so this seed can never
	// quietly disagree with what reconcileHold itself would render -- see
	// TestHoldSnapshotHandle_rendersAdHocJobName for the pinned format.
	hold.Status.SnapshotHandle = workspace.SnapshotJobName((&Reconciler{}).holdSnapshotHandle(sess, hold))

	_, r := newParkTestReconciler(t, sess, hold)
	snap := &fakeHoldSnapshotter{}
	r.Snapshotter = snap

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)
	assert.Zero(t, snap.snapshotCalls, "an already-recorded SnapshotHandle must not launch a second snapshot Job")
}

// TestReconcileSleep_heldSessionIsNotArchived proves a hold suppresses idle
// GC: an active SessionHold naming a channel session that is otherwise past
// its idle-sleep grace must stop reconcileSleep from reaping it. Without the
// guard the sleep-due condition below would fire and reap the session, which
// is exactly the "evidence expires on a timer" failure the design rejected
// (see the note ruling out reusing Failed + FailedSandboxReapGrace).
func TestReconcileSleep_heldSessionIsNotArchived(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	lastIdleAt := metav1.NewTime(now.Add(-time.Hour)) // well past a 10m sleepAfter grace
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:      spiceboxv1alpha1.AgentSessionPhaseHeld,
			LastIdleAt: &lastIdleAt,
		},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
	}
	_, r := newParkTestReconciler(t, sess, hold)
	r.DefaultSessionSleepAfter = 10 * time.Minute
	r.Now = func() time.Time { return now }

	slept, requeue, err := r.reconcileSleep(ctx, sess, &spiceboxv1alpha1.AgentClass{})
	require.NoError(t, err)
	assert.False(t, slept, "a held session must not be archived out from under a forensic review")
	assert.Zero(t, requeue, "a held session needs no sleep requeue")
}

// TestReconcileArchive_activeHoldBlocksArchival proves the archive sweep
// respects a hold the same way reconcileSleep does: an active SessionHold
// naming a channel session that is otherwise past its archiveAfter grace must
// stop reconcileArchive from transitioning it to Succeeded. Archival is
// terminal and the lifecycle machine's terminal-sticky guard means a later
// Held event could never move the session off Succeeded once archived, so
// this guard is what stops the hold from silently losing.
func TestReconcileArchive_activeHoldBlocksArchival(t *testing.T) {
	ctx := context.Background()
	lastIdleAt := metav1.NewTime(time.Now().Add(-time.Hour)) // well past a 10m archiveAfter grace
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:      spiceboxv1alpha1.AgentSessionPhaseIdle,
			LastIdleAt: &lastIdleAt,
		},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
	}
	_, r := newParkTestReconciler(t, sess, hold)
	r.DefaultChannelArchiveAfter = 10 * time.Minute

	transitioned, requeue, err := r.reconcileArchive(ctx, sess, &spiceboxv1alpha1.AgentClass{})
	require.NoError(t, err)
	assert.False(t, transitioned, "an active hold must block the archive sweep")
	assert.Zero(t, requeue, "a held session needs no archive requeue")
}

// TestReconcileArchive_releasedHoldDoesNotBlockArchival proves the converse:
// a released hold must not keep suppressing archival forever -- once a human
// has cleared the card, the ordinary long-idle sweep resumes.
func TestReconcileArchive_releasedHoldDoesNotBlockArchival(t *testing.T) {
	ctx := context.Background()
	lastIdleAt := metav1.NewTime(time.Now().Add(-time.Hour))
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:      spiceboxv1alpha1.AgentSessionPhaseIdle,
			LastIdleAt: &lastIdleAt,
		},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseReleased},
	}
	c, r := newParkTestReconciler(t, sess, hold)
	r.DefaultChannelArchiveAfter = 10 * time.Minute

	transitioned, requeue, err := r.reconcileArchive(ctx, sess, &spiceboxv1alpha1.AgentClass{})
	require.NoError(t, err)
	assert.True(t, transitioned, "a released hold must not block archival")
	assert.Zero(t, requeue)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &got))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, got.Status.Phase)
}

// TestReconcileExpiration_activeHoldBlocksExpiration proves the wall-clock
// expiration sweep respects a hold the same way reconcileSleep and
// reconcileArchive do: an active SessionHold naming a session that is
// otherwise past its sessionExpiration cap must stop reconcileExpiration from
// transitioning it to Failed.
func TestReconcileExpiration_activeHoldBlocksExpiration(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	startedAt := metav1.NewTime(now.Add(-time.Hour)) // well past a 30m sessionExpiration cap
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:     spiceboxv1alpha1.AgentSessionPhaseIdle,
			StartedAt: &startedAt,
			EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{
				Budget: spiceboxv1alpha1.BudgetConfig{SessionExpiration: metav1.Duration{Duration: 30 * time.Minute}},
			},
		},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseActive},
	}
	_, r := newParkTestReconciler(t, sess, hold)
	r.Now = func() time.Time { return now }

	transitioned, requeue, err := r.reconcileExpiration(ctx, sess)
	require.NoError(t, err)
	assert.False(t, transitioned, "an active hold must block the expiration sweep")
	assert.Zero(t, requeue, "a held session needs no expiration requeue")
}

// TestReconcileExpiration_releasedHoldDoesNotBlockExpiration proves the
// converse: a released hold must not keep suppressing the wall-clock
// lifetime cap forever.
func TestReconcileExpiration_releasedHoldDoesNotBlockExpiration(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	startedAt := metav1.NewTime(now.Add(-time.Hour))
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:     spiceboxv1alpha1.AgentSessionPhaseIdle,
			StartedAt: &startedAt,
			EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{
				Budget: spiceboxv1alpha1.BudgetConfig{SessionExpiration: metav1.Duration{Duration: 30 * time.Minute}},
			},
		},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseReleased},
	}
	c, r := newParkTestReconciler(t, sess, hold)
	r.Now = func() time.Time { return now }

	transitioned, requeue, err := r.reconcileExpiration(ctx, sess)
	require.NoError(t, err)
	assert.True(t, transitioned, "a released hold must not block expiration")
	assert.Zero(t, requeue)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &got))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
}

// TestReconcile_activeHoldWinsRaceAgainstArchival proves the call-site fix:
// a channel-attached, Idle, non-waking session that is BOTH past its
// archiveAfter grace AND named by an active SessionHold must end up Held
// through a full Reconcile, not Succeeded. Before reconcileHold's call site
// moved above the idle-sweep block, that block (controller.go) always
// returned from Reconcile before reconcileHold ever ran, so
// reconcileArchive won this race, permanently archived the session, and the
// lifecycle machine's terminal-sticky guard meant a later Held event could
// never move it off Succeeded -- exactly the "evidence expires on a timer"
// failure this design exists to prevent.
func TestReconcile_activeHoldWinsRaceAgainstArchival(t *testing.T) {
	f := newProvisioningGateFixture(t)           // Idle, channel-attached, LastIdleAt = -2m, resolvable class
	f.r.DefaultChannelArchiveAfter = time.Minute // past due against LastIdleAt=-2m

	// Pre-seed TrippedAt and the (idempotent) SnapshotHandle so this pass's
	// reconcileHold has nothing left to do but park+reap -- this test is about
	// call-site ordering against the archive sweep, not snapshot sequencing
	// (already covered by TestReconcileHold_snapshotCompletes_reapsAndRecordsHandle).
	trippedAt := metav1.NewTime(f.now)
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "default"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: "s1"},
			Reason:     "denial streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{
			Phase:     spiceboxv1alpha1.SessionHoldPhaseActive,
			TrippedAt: &trippedAt,
		},
	}
	hold.Status.SnapshotHandle = workspace.SnapshotJobName((&Reconciler{}).holdSnapshotHandle(f.sess, hold))
	require.NoError(t, f.c.Create(context.Background(), hold))

	f.reconcile(t)

	got := f.getSession(t)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseHeld, got.Status.Phase,
		"an active hold must win the race against the archive sweep")
}

// TestReconcile_activeHoldOnRunningSessionParksAndReaps proves the
// call-site move is safe for a session OUTSIDE the idle-sweep block too.
// Moving reconcileHold above that block (fix round 1) means a hold on a
// Running, channel-attached, mid-turn session now intercepts BEFORE the
// sidecar toolbox, content-guard detector, wake-annotation, runner-pod and
// pod-phase steps -- all of which used to run first, ahead of the old
// (later) call site. Those steps are reachable-but-skipped for a held
// session, which is the point of containment, but nothing had exercised that
// through a full Reconcile before this test: an active hold must still land
// the session on Held with its pods reaped, and Reconcile must not error or
// panic from skipping straight past them.
//
// The AgentClass here declares NO tool bundles, so step 2 (bundle
// provisioning, which runs before reconcileHold in both the old and new
// call-site positions and so is unaffected by the move) takes the vacuous
// NoBundles path regardless of phase -- keeping this test scoped to the
// hold-interception question rather than bundle-readiness plumbing. The
// pre-existing bundle SpiceboxSession and runner pod below simulate state a
// prior Pending/Running pass already provisioned, which is what
// reconcileHold's reap step must tear down.
func TestReconcile_activeHoldOnRunningSessionParksAndReaps(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "cls",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the task"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "chan-1", Kind: "fake", Key: "thread-1",
			},
		},
	}
	gitName := BundleSessionName(sess, spiceboxv1alpha1.ToolBundle{Name: "git"})
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase: spiceboxv1alpha1.AgentSessionPhaseRunning,
		BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
			{Name: "git", SpiceboxSessionName: gitName},
		},
	}

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeAgent,
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 50, MaxTokens: 100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAllReferencesResolve, LastTransitionTime: metav1.Now(),
		}}},
	}

	bundle := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: gitName, Namespace: "default"},
		Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "toolbelt"},
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}

	// TrippedAt and SnapshotHandle pre-seeded so this pass's reconcileHold has
	// nothing left to do but park+reap -- this test is about call-site
	// ordering, not snapshot sequencing (covered by
	// TestReconcileHold_snapshotCompletes_reapsAndRecordsHandle).
	trippedAt := metav1.NewTime(now)
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "default"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: "s1"},
			Reason:     "denial streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{
			Phase:     spiceboxv1alpha1.SessionHoldPhaseActive,
			TrippedAt: &trippedAt,
		},
	}
	hold.Status.SnapshotHandle = workspace.SnapshotJobName((&Reconciler{}).holdSnapshotHandle(sess, hold))

	c := buildFakeClient(t, sess, class, bundle, runner, hold)
	r := &Reconciler{
		Client:    c,
		APIReader: c,
		Tokens:    tokens.NewRegistry(),
		Memory:    memory.NewLocal(inmem.NewBackend()),
		Now:       func() time.Time { return now },
	}
	r.RunnerFactory = &PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}

	ctx := memory.WithSystemApproval(context.Background(), "test")
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}})
	require.NoError(t, err, "an active hold on a Running session must not error or panic Reconcile")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "s1"}, &got))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseHeld, got.Status.Phase,
		"an active hold must park a Running session at Held even though reconcileHold now runs before the sidecar/detector/wake/runner-pod steps")

	var gotBundle spiceboxv1alpha1.SpiceboxSession
	err = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: gitName}, &gotBundle)
	assert.True(t, apierrors.IsNotFound(err), "the bundle SpiceboxSession must still be reaped, got err=%v", err)

	var gotRunner corev1.Pod
	err = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: RunnerPodName(sess)}, &gotRunner)
	assert.True(t, apierrors.IsNotFound(err), "the runner pod must still be reaped, got err=%v", err)
}
