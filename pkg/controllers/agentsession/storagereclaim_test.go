// pkg/controllers/agentsession/storagereclaim_test.go
//
// White-box unit tests (fake client + injected clock) for
// reconcileStorageReclaim: a TERMINAL AgentSession past its storage-retention
// deadline has its workspace + snapshot-store PVCs deleted and the
// StorageReclaimed condition set. Mirrors expiration_test.go's fixture
// pattern; the sweep is exercised directly (white-box) rather than through
// the full Reconcile.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

// storageReclaimFixture wires a reconciler over a fake client pre-loaded with
// a session in the given phase, finished finishedDelta relative to the
// injected clock, plus that session's workspace and snapshot-store PVCs (so a
// deletion is observable as absence, not merely as a lack of error).
type storageReclaimFixture struct {
	r    *Reconciler
	c    client.Client
	sess *spiceboxv1alpha1.AgentSession
	now  time.Time
}

// noFinishedAt leaves Status.FinishedAt nil — the defensive case where a
// terminal phase was reached without the stamp.
const noFinishedAt = time.Duration(1<<63 - 1)

func newStorageReclaimFixture(t *testing.T, phase string, finishedDelta, defaultRetention time.Duration, extra ...client.Object) storageReclaimFixture {
	t.Helper()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			// CreationTimestamp is well in the past so the FinishedAt-nil
			// fallback case has a deterministic, already-elapsed reference.
			CreationTimestamp: metav1.NewTime(now.Add(-30 * 24 * time.Hour)),
			Finalizers:        []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "cls"},
	}
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{Phase: phase}
	if finishedDelta != noFinishedAt {
		finished := metav1.NewTime(now.Add(finishedDelta))
		sess.Status.FinishedAt = &finished
	}

	objs := append([]client.Object{sess,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: podspec.WorkspaceClaimName(sess)}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: podspec.SnapshotStoreClaimName(sess)}},
	}, extra...)

	c := buildFakeClient(t, objs...)
	r := &Reconciler{
		Client:                         c,
		APIReader:                      c,
		Tokens:                         tokens.NewRegistry(),
		Memory:                         memory.NewLocal(inmem.NewBackend()),
		Now:                            func() time.Time { return now },
		DefaultSessionStorageRetention: defaultRetention,
	}
	return storageReclaimFixture{r: r, c: c, sess: sess, now: now}
}

func (f storageReclaimFixture) pvcExists(t *testing.T, name string) bool {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	err := f.c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &pvc)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func (f storageReclaimFixture) getSession(t *testing.T) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(memory.WithSystemApproval(context.Background(), "test"),
		client.ObjectKey{Namespace: "default", Name: "s1"}, &got))
	return &got
}

func TestStorageReclaim_PastDeadlineDeletesBothClaimsAndSetsCondition(t *testing.T) {
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		-4*24*time.Hour, 72*time.Hour) // finished 4d ago, retention 3d: past

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)

	assert.False(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "workspace claim deleted")
	assert.False(t, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)), "snapshot-store claim deleted")

	got := f.getSession(t)
	cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStorageReclaimed)
	require.NotNil(t, cond, "StorageReclaimed condition recorded")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestStorageReclaim_BeforeDeadlineRequeuesAndDeletesNothing(t *testing.T) {
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseFailed,
		-24*time.Hour, 72*time.Hour) // finished 1d ago, retention 3d: 2d left

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, 48*time.Hour, requeue, "requeue at the retention deadline")
	assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)))
	assert.True(t, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)))
}

func TestStorageReclaim_DisabledAndNonTerminalDoNothing(t *testing.T) {
	cases := []struct {
		name      string
		phase     string
		retention time.Duration
	}{
		{name: "retention 0 disables the sweep entirely", phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded, retention: 0},
		{name: "a non-terminal session is never swept, whatever its age", phase: spiceboxv1alpha1.AgentSessionPhaseIdle, retention: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStorageReclaimFixture(t, tc.phase, -30*24*time.Hour, tc.retention)

			requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
			require.NoError(t, err)
			assert.Equal(t, time.Duration(0), requeue)
			assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "claims untouched")
			assert.True(t, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)))

			got := f.getSession(t)
			assert.Nil(t, conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStorageReclaimed))
		})
	}
}

func TestStorageReclaim_MissingClaimsAreIdempotentlyFine(t *testing.T) {
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		-4*24*time.Hour, 72*time.Hour)
	// Delete both claims up front: the sweep must treat NotFound as done.
	require.NoError(t, f.c.Delete(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: podspec.WorkspaceClaimName(f.sess)}}))
	require.NoError(t, f.c.Delete(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: podspec.SnapshotStoreClaimName(f.sess)}}))

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)

	got := f.getSession(t)
	cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStorageReclaimed)
	require.NotNil(t, cond, "condition still recorded so the sweep never re-runs")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

// withReclaimedMarker stamps a prior sweep's StorageReclaimed=True onto the
// fixture's session — the state a woken session carries into its second life.
func (f *storageReclaimFixture) withReclaimedMarker(t *testing.T, msg string) {
	t.Helper()
	patched := f.sess.DeepCopy()
	conditions.Set(f.sess, &patched.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionStorageReclaimed, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonAgentSessionStorageReclaimed, Message: msg,
	})
	require.NoError(t, f.c.Status().Update(context.Background(), patched))
	f.sess = f.getSession(t)
}

func TestStorageReclaim_ReclaimedMarkerDoesNotStrandReprovisionedClaims(t *testing.T) {
	// The leak this guards: a swept session wakes, re-provisions both claims
	// (ensure-if-missing) and finishes AGAIN. Treating the marker as
	// write-once stranded that second lifetime's storage forever on the
	// node-local workspace disk — precisely the accumulation the sweep exists
	// to prevent. The marker must gate churn, never the sweep; the deadline
	// is always measured from the CURRENT FinishedAt, so the fresh storage
	// gets its own full retention window and no decision is ever stale.
	cases := []struct {
		name          string
		finishedDelta time.Duration
		wantRequeue   time.Duration
		wantClaims    bool
	}{
		{
			name:          "re-finished past the new deadline: the re-provisioned claims are reclaimed again",
			finishedDelta: -4 * 24 * time.Hour,
			wantRequeue:   0,
			wantClaims:    false,
		},
		{
			name:          "re-finished inside the new window: the re-provisioned claims are kept, requeued at the deadline",
			finishedDelta: -24 * time.Hour,
			wantRequeue:   48 * time.Hour,
			wantClaims:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded,
				tc.finishedDelta, 72*time.Hour)
			f.withReclaimedMarker(t, "swept in a previous lifetime")

			requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
			require.NoError(t, err)
			assert.Equal(t, tc.wantRequeue, requeue)
			assert.Equal(t, tc.wantClaims, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "workspace claim")
			assert.Equal(t, tc.wantClaims, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)), "snapshot-store claim")
		})
	}
}

func TestStorageReclaim_ReclaimedWithNothingLeftWritesNothing(t *testing.T) {
	// Steady state after a sweep: marker True, both claims already gone. The
	// sweep must short-circuit without re-logging or re-writing status, or a
	// terminal session churns its status on every reconcile forever. The
	// untouched Message is the observable — a re-run would overwrite it with
	// the generated "claims deleted ..." text.
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		-4*24*time.Hour, 72*time.Hour)
	for _, n := range []string{podspec.WorkspaceClaimName(f.sess), podspec.SnapshotStoreClaimName(f.sess)} {
		require.NoError(t, f.c.Delete(context.Background(), &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: n}}))
	}
	f.withReclaimedMarker(t, "swept in a previous lifetime")

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)

	cond := conditions.Find(f.getSession(t).Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStorageReclaimed)
	require.NotNil(t, cond)
	assert.Equal(t, "swept in a previous lifetime", cond.Message, "sweep wrote nothing on the no-op pass")
}

func TestStorageReclaim_ClassOverrideBeatsOperatorDefault(t *testing.T) {
	// Operator default 3d would say "past deadline" (finished 4d ago); the
	// class stretches retention to 7d, so nothing is deleted yet.
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cls"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Channels: &spiceboxv1alpha1.ChannelsConfig{
				StorageRetention: metav1.Duration{Duration: 7 * 24 * time.Hour},
			},
		},
	}
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		-4*24*time.Hour, 72*time.Hour, ac)

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, 3*24*time.Hour, requeue, "the class's 7d retention leaves 3d")
	assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)))
}

func TestStorageReclaim_DeletedClassFallsBackToOperatorDefault(t *testing.T) {
	// No AgentClass object at all: the sweep must still run on the operator
	// default — a terminal session whose class was deleted reclaims, same rule
	// archive follows.
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		-4*24*time.Hour, 72*time.Hour)

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)
	assert.False(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)))
}

func TestEffectiveStorageRetention(t *testing.T) {
	const grace = 15 * time.Minute
	cases := []struct {
		name       string
		base       time.Duration
		nodePinned bool
		grace      time.Duration
		want       time.Duration
	}{
		{"non-node-pinned: base unchanged", 72 * time.Hour, false, grace, 72 * time.Hour},
		{"node-pinned, grace 0: cap disabled, base unchanged", 72 * time.Hour, true, 0, 72 * time.Hour},
		{"node-pinned: long base capped to the short grace", 72 * time.Hour, true, grace, grace},
		{"node-pinned: an already-shorter base is honored (min)", 5 * time.Minute, true, grace, 5 * time.Minute},
		{"node-pinned: 'disabled' base (0) still reclaims on the grace", 0, true, grace, grace},
		{"node-pinned: negative base still reclaims on the grace", -1, true, grace, grace},
		{"non-node-pinned, disabled base stays disabled", 0, false, grace, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, effectiveStorageRetention(tc.base, tc.nodePinned, tc.grace))
		})
	}
}

func TestStorageReclaim_NodePinnedClassUsesShortGrace(t *testing.T) {
	// Node-pinned (local-path) workspace class: the long 72h default must be
	// capped to the short reclaim grace so the terminal session's scratch
	// volumes cannot pile onto one node's disk.
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		-20*time.Minute, 72*time.Hour) // finished 20m ago
	f.r.WorkspaceStorageClass = cloud.BundledWorkspaceStorageClass
	f.r.NodePinnedStorageReclaimGrace = 15 * time.Minute // 20m > 15m: past

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)
	assert.False(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "workspace claim reclaimed on the short grace")
	assert.False(t, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)))
}

func TestStorageReclaim_NodePinnedClassRequeuesWithinGrace(t *testing.T) {
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseFailed,
		-5*time.Minute, 72*time.Hour) // finished 5m ago
	f.r.WorkspaceStorageClass = cloud.BundledWorkspaceStorageClass
	f.r.NodePinnedStorageReclaimGrace = 15 * time.Minute // 5m < 15m: 10m left

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, requeue, "requeue at the short-grace deadline")
	assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "debug window preserved within the grace")
}

func TestStorageReclaim_SharedClassKeepsLongRetention(t *testing.T) {
	// A cross-node RWX (Filestore) class puts no node's disk at risk, so the
	// short grace must NOT apply — the long default retention still governs.
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		-20*time.Minute, 72*time.Hour)
	f.r.WorkspaceStorageClass = "enterprise-multishare-rwx"
	f.r.NodePinnedStorageReclaimGrace = 15 * time.Minute

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.InDelta(t, (72*time.Hour - 20*time.Minute).Seconds(), requeue.Seconds(), 1,
		"shared class keeps the long retention deadline")
	assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "not reclaimed early on a shared class")
}

func TestStorageReclaim_NilFinishedAtFallsBackToCreation(t *testing.T) {
	// Defensive: terminal with no FinishedAt stamp. The 30d-old creation
	// timestamp is the reference, so a 3d retention is long past.
	f := newStorageReclaimFixture(t, spiceboxv1alpha1.AgentSessionPhaseFailed,
		noFinishedAt, 72*time.Hour)

	requeue, err := f.r.reconcileStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)
	assert.False(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)))
}

// newIdleReclaimFixture wires a reconciler over a fake client holding a SLEPT,
// Idle session (SleptAt = now+sleptDelta; noFinishedAt leaves it nil, the
// not-yet-slept case) plus its two scratch claims, with the node-pinned
// workspace class and IdleStorageReclaimAfter set. FinishedAt stays nil — an
// idle session is not terminal. Mirrors newStorageReclaimFixture's shape.
func newIdleReclaimFixture(t *testing.T, sleptDelta, idleReclaimAfter time.Duration, extra ...client.Object) storageReclaimFixture {
	t.Helper()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			CreationTimestamp: metav1.NewTime(now.Add(-30 * 24 * time.Hour)),
			Finalizers:        []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "cls"},
	}
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle}
	if sleptDelta != noFinishedAt {
		slept := metav1.NewTime(now.Add(sleptDelta))
		sess.Status.SleptAt = &slept
	}

	objs := append([]client.Object{sess,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: podspec.WorkspaceClaimName(sess)}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: podspec.SnapshotStoreClaimName(sess)}},
	}, extra...)

	c := buildFakeClient(t, objs...)
	r := &Reconciler{
		Client:                  c,
		APIReader:               c,
		Tokens:                  tokens.NewRegistry(),
		Memory:                  memory.NewLocal(inmem.NewBackend()),
		Now:                     func() time.Time { return now },
		WorkspaceStorageClass:   cloud.BundledWorkspaceStorageClass, // node-pinned; shared-class test overrides
		IdleStorageReclaimAfter: idleReclaimAfter,
	}
	return storageReclaimFixture{r: r, c: c, sess: sess, now: now}
}

func TestIdleStorageReclaim_SleptPastGraceDeletesBothClaimsAndStaysIdle(t *testing.T) {
	// Slept 40m ago, 30m grace: past. The node-pinned parked session's scratch
	// is reclaimed, but the session stays Idle and wakeable (wake re-provisions).
	f := newIdleReclaimFixture(t, -40*time.Minute, 30*time.Minute)

	requeue, err := f.r.reconcileIdleStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)
	assert.False(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "workspace claim reclaimed")
	assert.False(t, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)), "snapshot-store claim reclaimed")

	got := f.getSession(t)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "session stays Idle/wakeable")
	cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStorageReclaimed)
	require.NotNil(t, cond, "StorageReclaimed recorded so a re-sleep does not churn status")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestIdleStorageReclaim_WithinGraceRequeuesAndKeepsClaims(t *testing.T) {
	// Slept 10m ago, 30m grace: 20m left. Nothing deleted; requeue at the deadline.
	f := newIdleReclaimFixture(t, -10*time.Minute, 30*time.Minute)

	requeue, err := f.r.reconcileIdleStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, 20*time.Minute, requeue, "requeue at the idle-reclaim deadline")
	assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)))
	assert.True(t, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)))
}

func TestIdleStorageReclaim_NotYetSleptIsNoOp(t *testing.T) {
	// SleptAt nil: the session is Idle but its pods are still warm. Reclaiming
	// its storage now could race a still-running snapshot, so it must not fire.
	f := newIdleReclaimFixture(t, noFinishedAt, 30*time.Minute)

	requeue, err := f.r.reconcileIdleStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)
	assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "claims untouched until slept")
	assert.True(t, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)))
}

func TestIdleStorageReclaim_SharedClassIsExempt(t *testing.T) {
	// A cross-node RWX (Filestore) class puts no single node's disk at risk, so
	// the idle sweep must never touch it, however long the session has slept.
	f := newIdleReclaimFixture(t, -40*time.Minute, 30*time.Minute)
	f.r.WorkspaceStorageClass = "enterprise-multishare-rwx"

	requeue, err := f.r.reconcileIdleStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)
	assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "shared-class scratch kept")
	assert.True(t, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)))
}

func TestIdleStorageReclaim_DisabledIsNoOp(t *testing.T) {
	// IdleStorageReclaimAfter 0: the sweep is off, even for a long-slept session.
	f := newIdleReclaimFixture(t, -40*time.Minute, 0)

	requeue, err := f.r.reconcileIdleStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)
	assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)))
	assert.True(t, f.pvcExists(t, podspec.SnapshotStoreClaimName(f.sess)))
}

func TestIdleStorageReclaim_NonIdlePhaseIsNoOp(t *testing.T) {
	// Defense in depth: SleptAt is only ever set while Idle, but should a
	// non-Idle session carry one, the sweep must not pull storage out from
	// under a phase that may still be using it.
	f := newIdleReclaimFixture(t, -40*time.Minute, 30*time.Minute)
	f.sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning

	requeue, err := f.r.reconcileIdleStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)
	assert.True(t, f.pvcExists(t, podspec.WorkspaceClaimName(f.sess)), "running session's claims untouched")
}

func TestIdleStorageReclaim_MissingClaimsAreIdempotentlyFine(t *testing.T) {
	// Past grace but both claims already gone (a prior pass, or a wake that
	// never re-provisioned): NotFound is success, no error, nothing to requeue.
	f := newIdleReclaimFixture(t, -40*time.Minute, 30*time.Minute)
	for _, n := range []string{podspec.WorkspaceClaimName(f.sess), podspec.SnapshotStoreClaimName(f.sess)} {
		require.NoError(t, f.c.Delete(context.Background(), &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: n}}))
	}

	requeue, err := f.r.reconcileIdleStorageReclaim(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeue)
}
