// pkg/controllers/agentsession/sleep_reconcile_test.go
//
// White-box unit tests (fake client + injected clock) for reconcileSleep: an
// Idle, channel-attached AgentSession past its configured sleepAfter grace
// has its sandbox pods reaped while the session itself stays Idle (wakeable).
// Mirrors reap_test.go's fixture pattern; reconcileSleep is exercised
// directly (white-box) rather than through the full Reconcile, matching
// TestReapSessionPodsPreservesPVC's style — the surrounding Reconcile steps
// (AgentClass resolution, RBAC, tokens, sidecars, ...) are orthogonal to the
// sleep decision itself.
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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

// sleepFixture wires a reconciler over a fake client pre-loaded with an Idle,
// channel-attached AgentSession, its two bundle SpiceboxSessions, and its
// runner pod — the sandbox reconcileSleep tears down.
type sleepFixture struct {
	r       *Reconciler
	c       client.Client
	sess    *spiceboxv1alpha1.AgentSession
	class   *spiceboxv1alpha1.AgentClass
	bundles []string // SpiceboxSession names
	runner  string   // runner pod name
	now     time.Time
}

// newSleepFixture builds an Idle channel-attached session with LastIdleAt set
// lastIdleDelta relative to the fixture's injected clock (e.g. -1h = went
// idle an hour ago), a resolvable AgentClass, its two bundle SpiceboxSessions,
// and its runner pod.
func newSleepFixture(t *testing.T, lastIdleDelta time.Duration, sleepAfter time.Duration) sleepFixture {
	t.Helper()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	lastIdleAt := metav1.NewTime(now.Add(lastIdleDelta))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "cls",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "chan-1", Kind: "fake", Key: "thread-1",
			},
		},
	}
	gitName := BundleSessionName(sess, spiceboxv1alpha1.ToolBundle{Name: "git"})
	codeName := BundleSessionName(sess, spiceboxv1alpha1.ToolBundle{Name: "code"})
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase:      spiceboxv1alpha1.AgentSessionPhaseIdle,
		LastIdleAt: &lastIdleAt,
		BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
			{Name: "git", SpiceboxSessionName: gitName},
			{Name: "code", SpiceboxSessionName: codeName},
		},
	}

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
	}

	bundle := func(name string) *spiceboxv1alpha1.SpiceboxSession {
		return &spiceboxv1alpha1.SpiceboxSession{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "toolbelt"},
		}
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}

	c := buildFakeClient(t, sess, class, bundle(gitName), bundle(codeName), runner)
	r := &Reconciler{
		Client:                   c,
		APIReader:                c,
		Tokens:                   tokens.NewRegistry(),
		Memory:                   memory.NewLocal(inmem.NewBackend()),
		Now:                      func() time.Time { return now },
		DefaultSessionSleepAfter: sleepAfter,
	}
	r.RunnerFactory = &PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}
	return sleepFixture{r: r, c: c, sess: sess, class: class, bundles: []string{gitName, codeName}, runner: RunnerPodName(sess), now: now}
}

func (f sleepFixture) bundlesGone(t *testing.T) bool {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	for _, name := range f.bundles {
		err := f.c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &spiceboxv1alpha1.SpiceboxSession{})
		if !apierrors.IsNotFound(err) {
			return false
		}
	}
	err := f.c.Get(ctx, client.ObjectKey{Namespace: "default", Name: f.runner}, &corev1.Pod{})
	return apierrors.IsNotFound(err)
}

func (f sleepFixture) getSession(t *testing.T) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(memory.WithSystemApproval(context.Background(), "test"), client.ObjectKey{Namespace: "default", Name: "s1"}, &got))
	return &got
}

func TestReconcileSleepReapsIdleSession(t *testing.T) {
	f := newSleepFixture(t, -time.Hour, 10*time.Minute) // idle an hour ago, 10m grace: past due

	slept, requeueAfter, err := f.r.reconcileSleep(memory.WithSystemApproval(context.Background(), "test"), f.sess, f.class)
	require.NoError(t, err)
	assert.True(t, slept, "past-grace Idle session should sleep")
	assert.Zero(t, requeueAfter, "a sleep-reap returns no requeue")

	got := f.getSession(t)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "stays Idle (wakeable)")
	assert.NotNil(t, got.Status.SleptAt, "SleptAt stamped")
	assert.True(t, f.bundlesGone(t), "bundle SpiceboxSessions + runner pod reaped")
}

func TestReconcileSleepWithinGraceRequeues(t *testing.T) {
	f := newSleepFixture(t, -2*time.Minute, 10*time.Minute) // idle 2m ago, 10m grace: within grace

	slept, requeueAfter, err := f.r.reconcileSleep(memory.WithSystemApproval(context.Background(), "test"), f.sess, f.class)
	require.NoError(t, err)
	assert.False(t, slept, "within-grace Idle session should not sleep yet")
	assert.Equal(t, 8*time.Minute, requeueAfter, "requeue ~= grace minus elapsed")

	got := f.getSession(t)
	assert.Nil(t, got.Status.SleptAt, "SleptAt not stamped")
	assert.False(t, f.bundlesGone(t), "within grace the sandbox is kept warm")
}

func TestSleptSessionNotReReaped(t *testing.T) {
	f := newSleepFixture(t, -time.Hour, 10*time.Minute) // past due
	sleptAt := metav1.NewTime(f.now.Add(-30 * time.Minute))
	f.sess.Status.SleptAt = &sleptAt
	require.NoError(t, f.c.Status().Update(memory.WithSystemApproval(context.Background(), "test"), f.sess), "seed already-slept status")

	slept, requeueAfter, err := f.r.reconcileSleep(memory.WithSystemApproval(context.Background(), "test"), f.sess, f.class)
	require.NoError(t, err)
	assert.False(t, slept, "already-slept session should not be re-evaluated as due")
	assert.Zero(t, requeueAfter)

	// No-op: reconcileSleep must not have touched the (already-reaped-or-not)
	// sandbox on this pass since it never got past the due check.
	assert.False(t, f.bundlesGone(t), "not due: reconcileSleep must not reap")
}

// TestWakeFromSleptDoesNotStartRunnerBeforeBundles pins the fix for the
// wake-from-slept boot-crash bug: on the SAME reconcile pass that folds
// WakeRequested (Idle -> Pending), Reconcile must return before falling
// through to step 5b (RunnerFactory.Start). Step 2 (bundle provisioning) ran
// EARLIER in this same pass while the in-memory phase was still Idle, so it
// was gated off by sandboxProvisioningDesired — the bundle SpiceboxSessions
// the sleep reaper deleted are NOT re-created this pass. Starting the runner
// anyway would spawn it against sess.Status.BundleSessions entries that
// don't exist yet; the runner process Gets each one at startup
// (internal/cmd/runner/main.go) and errors into a boot crash loop. Reuses
// newProvisioningGateFixture (provisioning_gate_test.go): an Idle,
// channel-attached session whose bundle SpiceboxSessions are already absent,
// simulating post-reap state.
func TestWakeFromSleptDoesNotStartRunnerBeforeBundles(t *testing.T) {
	f := newProvisioningGateFixture(t)
	sleptAt := metav1.NewTime(f.now.Add(-30 * time.Minute))
	f.sess.Status.SleptAt = &sleptAt
	require.NoError(t, f.c.Status().Update(memory.WithSystemApproval(context.Background(), "test"), f.sess), "seed already-slept status")

	// Fresh wake annotation: no prior LastWakeAt, so shouldWake is true.
	updated := f.getSession(t)
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = f.now.Format(time.RFC3339Nano)
	require.NoError(t, f.c.Update(memory.WithSystemApproval(context.Background(), "test"), updated), "seed wake annotation")

	f.reconcile(t) // wake pass: applies WakeRequested (Idle -> Pending)

	// Key assertion: the runner pod must NOT exist yet. Pre-fix, block 5a
	// fell through to block 5b (RunnerFactory.Start) on this very pass,
	// creating the pod before the bundles it depends on were re-provisioned.
	err := f.c.Get(memory.WithSystemApproval(context.Background(), "test"),
		client.ObjectKey{Namespace: "default", Name: RunnerPodName(f.sess)}, &corev1.Pod{})
	assert.True(t, apierrors.IsNotFound(err), "runner pod must not be created before bundles are re-provisioned")

	got := f.getSession(t)
	assert.Nil(t, got.Status.SleptAt, "wake clears SleptAt on the same pass")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, got.Status.Phase, "wake pass folds phase to Pending")

	// Second reconcile (now Pending, from the top): step 2 re-provisions the
	// bundles before step 5b would ever start the runner.
	f.reconcile(t)
	assert.False(t, f.bundlesGone(t), "the Pending pass re-provisions the bundle SpiceboxSessions")
}

// TestSleepResumePreservesWorkspacePVC is the Task 8 shared-volume round
// trip: an Idle, channel-attached session that goes past its sleepAfter
// grace is reaped (bundle SpiceboxSessions + runner pod deleted) but must
// never touch the AgentSession-owned shared workspace PVC, and a subsequent
// wake must re-mount that SAME claim rather than deleting and recreating it.
//
// The fake client has no kubelet, so file-content persistence isn't
// observable here (that's the e2e round trip's job); this test instead
// proves PVC OBJECT identity survives the whole cycle by comparing the PVC's
// UID before sleep, immediately after the sleep-reap, and again after the
// wake→resume re-provision. The fake client does not auto-assign a UID on
// Create (unlike a real apiserver), so the seeded PVC is stamped with an
// explicit UID up front to make the comparison meaningful.
func TestSleepResumePreservesWorkspacePVC(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	lastIdleAt := metav1.NewTime(now.Add(-time.Hour)) // well past a 10m sleepAfter grace

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
	codeName := BundleSessionName(sess, spiceboxv1alpha1.ToolBundle{Name: "code"})
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase:      spiceboxv1alpha1.AgentSessionPhaseIdle,
		LastIdleAt: &lastIdleAt,
		BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
			{Name: "git", SpiceboxSessionName: gitName},
			{Name: "code", SpiceboxSessionName: codeName},
		},
	}

	class := provisioningGateClass() // Valid, resolvable, two tool bundles

	bundle := func(name string) *spiceboxv1alpha1.SpiceboxSession {
		return &spiceboxv1alpha1.SpiceboxSession{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "toolbelt"},
		}
	}
	runner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RunnerPodName(sess), Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:dev"}}},
	}

	// The AgentSession-owned shared workspace PVC, mirroring what
	// ensureWorkspacePVC creates during normal provisioning.
	pvc := BuildWorkspacePVC(sess, "rwx-storage", "2Gi")
	pvc.UID = "workspace-pvc-uid-1"

	c := buildFakeClient(t, sess, class, bundle(gitName), bundle(codeName), runner, pvc)
	r := &Reconciler{
		Client:                   c,
		APIReader:                c,
		Tokens:                   tokens.NewRegistry(),
		Memory:                   memory.NewLocal(inmem.NewBackend()),
		Now:                      func() time.Time { return now },
		DefaultSessionSleepAfter: 10 * time.Minute,
		WorkspaceStorageClass:    "rwx-storage",
	}
	r.RunnerFactory = &PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}

	ctx := memory.WithSystemApproval(context.Background(), "test")
	reconcile := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}})
		require.NoError(t, err, "Reconcile")
	}
	getSession := func() *spiceboxv1alpha1.AgentSession {
		t.Helper()
		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "s1"}, &got))
		return &got
	}
	bundlesGone := func() bool {
		t.Helper()
		for _, name := range []string{gitName, codeName} {
			err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &spiceboxv1alpha1.SpiceboxSession{})
			if !apierrors.IsNotFound(err) {
				return false
			}
		}
		return true
	}
	pvcUID := func() types.UID {
		t.Helper()
		var got corev1.PersistentVolumeClaim
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: podspec.WorkspaceClaimName(sess)}, &got),
			"workspace PVC must still exist")
		return got.UID
	}
	const wantUID = types.UID("workspace-pvc-uid-1")
	require.Equal(t, wantUID, pvcUID(), "sanity: seeded PVC UID readable before sleep")

	// --- Sleep: the past-grace Idle session reaps its sandbox pods ---
	reconcile()
	got := getSession()
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "sleep keeps the session Idle (wakeable)")
	assert.NotNil(t, got.Status.SleptAt, "SleptAt stamped")
	assert.True(t, bundlesGone(), "bundle SpiceboxSessions reaped on sleep")
	assert.Equal(t, wantUID, pvcUID(), "workspace PVC survives sleep (same UID, not recreated)")

	// --- Wake: a fresh wake annotation folds Idle -> Pending on this pass ---
	updated := getSession()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = now.Format(time.RFC3339Nano)
	require.NoError(t, c.Update(ctx, updated), "seed wake annotation")
	reconcile()
	got = getSession()
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, got.Status.Phase, "wake pass folds phase to Pending")
	assert.Nil(t, got.Status.SleptAt, "wake clears SleptAt so the session can sleep again later")

	// --- Resume: the following Pending pass re-provisions the bundles ---
	reconcile()
	assert.False(t, bundlesGone(), "resume re-creates the bundle SpiceboxSessions")
	assert.Equal(t, wantUID, pvcUID(), "same PVC re-mounted (UID unchanged across the whole sleep->resume round trip)")
}
