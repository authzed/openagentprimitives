// pkg/controllers/agentsession/reap_test.go
//
// White-box unit tests for the terminal-session pod reaper: a session that
// reached a terminal phase (Succeeded immediately; Failed after a grace)
// keeps its bundle SpiceboxSessions + runner pod + detector/cosidecar pods
// Running until the AgentSession object is GC'd by retention (slow),
// stranding CPU. Once eligible, the reconciler tears those pods down while
// KEEPING the AgentSession (status/conditions/bundleSessions) plus its PVCs,
// Secrets, RBAC, and SpiceDB relationships for debugging.
//
// The pure terminalReapAction test pins the gating boundary (Succeeded reaps
// immediately, Failed after grace, disabled, non-terminal, no finishedAt,
// within-grace, past-grace) without a client. The Reconcile-driven tests
// (fake client + injected clock) pin the effectful behavior: the bundle
// SpiceboxSessions + runner pod + detector pods are deleted, PVCs and the
// AgentSession survive, within-grace requeues with the remaining time, and a
// second pass after the reap is an idempotent no-op (never re-provisions).
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

// TestTerminalReapAction pins the pure gating decision: Succeeded reaps
// immediately regardless of finishedAt; Failed reaps only after finishedAt +
// failedGrace has elapsed (requeueing with the remaining time within grace);
// a non-terminal phase or a disabled (<=0) grace on the Failed path is "none"
// (leave the sandbox alone).
func TestTerminalReapAction(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ft := func(d time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(d)); return &t }
	ct := func(d time.Duration) metav1.Time { return metav1.NewTime(now.Add(d)) }
	cases := []struct {
		name          string
		phase         string
		finishedAt    *metav1.Time
		created       metav1.Time
		grace         time.Duration
		want          reapAction
		wantRemaining time.Duration
	}{
		{"Succeeded: reap immediately", spiceboxv1alpha1.AgentSessionPhaseSucceeded, ft(-1 * time.Hour), ct(-2 * time.Hour), time.Minute, reapActionReap, 0},
		{"Succeeded with nil finishedAt: still reap", spiceboxv1alpha1.AgentSessionPhaseSucceeded, nil, ct(-2 * time.Hour), time.Minute, reapActionReap, 0},
		{"Failed within grace: requeue", spiceboxv1alpha1.AgentSessionPhaseFailed, ft(-30 * time.Second), ct(-2 * time.Hour), time.Minute, reapActionRequeue, 30 * time.Second},
		{"Failed within grace: requeue with correct remaining", spiceboxv1alpha1.AgentSessionPhaseFailed, ft(-20 * time.Second), ct(-2 * time.Hour), 60 * time.Second, reapActionRequeue, 40 * time.Second},
		{"Failed past grace: reap", spiceboxv1alpha1.AgentSessionPhaseFailed, ft(-2 * time.Minute), ct(-2 * time.Hour), time.Minute, reapActionReap, 0},
		{"Failed exactly at grace: reap", spiceboxv1alpha1.AgentSessionPhaseFailed, ft(-1 * time.Minute), ct(-2 * time.Hour), time.Minute, reapActionReap, 0},
		{"Failed grace<=0: disabled", spiceboxv1alpha1.AgentSessionPhaseFailed, ft(-2 * time.Minute), ct(-2 * time.Hour), 0, reapActionNone, 0},
		// A terminal phase reached without a finishedAt stamp (e.g. the lifecycle
		// fold to Failed at controller.go's steady-state phase write) must still
		// reap off the creation timestamp — mirroring reconcileStorageReclaim's
		// same fallback — instead of parking its sandbox pods on a node forever.
		{"Failed nil finishedAt, created past grace: reap off creation", spiceboxv1alpha1.AgentSessionPhaseFailed, nil, ct(-2 * time.Minute), time.Minute, reapActionReap, 0},
		{"Failed nil finishedAt, created within grace: requeue off creation", spiceboxv1alpha1.AgentSessionPhaseFailed, nil, ct(-30 * time.Second), time.Minute, reapActionRequeue, 30 * time.Second},
		{"Idle: not a terminal reap", spiceboxv1alpha1.AgentSessionPhaseIdle, ft(-2 * time.Minute), ct(-2 * time.Hour), time.Minute, reapActionNone, 0},
		{"Running: not a terminal reap", spiceboxv1alpha1.AgentSessionPhaseRunning, ft(-2 * time.Minute), ct(-2 * time.Hour), time.Minute, reapActionNone, 0},
		{"Pending: not a terminal reap", spiceboxv1alpha1.AgentSessionPhasePending, ft(-2 * time.Minute), ct(-2 * time.Hour), time.Minute, reapActionNone, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, remaining := terminalReapAction(tc.phase, tc.finishedAt, tc.created, tc.grace, now)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantRemaining, remaining)
		})
	}
}

// reapFixture wires a reconciler over a fake client pre-loaded with a Failed
// session (finalizer present, finishedAt at now+finishedDelta), its two bundle
// SpiceboxSessions, and its runner pod — the sandbox the reaper tears down.
type reapFixture struct {
	r       *Reconciler
	c       client.Client
	sess    *spiceboxv1alpha1.AgentSession
	bundles []string // SpiceboxSession names
	runner  string   // runner pod name
}

// newReapFixture builds the fixture for the given phase/grace. finishedDelta is
// added to now to place status.finishedAt relative to the injected clock (e.g.
// -2m = failed 2 minutes ago).
func newReapFixture(t *testing.T, phase string, grace, finishedDelta time.Duration) reapFixture {
	t.Helper()
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "cls"},
	}
	gitName := BundleSessionName(sess, spiceboxv1alpha1.ToolBundle{Name: "git"})
	codeName := BundleSessionName(sess, spiceboxv1alpha1.ToolBundle{Name: "code"})
	finishedAt := metav1.NewTime(now.Add(finishedDelta))
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase:      phase,
		FinishedAt: &finishedAt,
		BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
			{Name: "git", SpiceboxSessionName: gitName},
			{Name: "code", SpiceboxSessionName: codeName},
		},
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

	c := buildFakeClient(t, sess, bundle(gitName), bundle(codeName), runner)
	r := &Reconciler{
		Client:                 c,
		APIReader:              c,
		Tokens:                 tokens.NewRegistry(),
		Memory:                 memory.NewLocal(inmem.NewBackend()),
		Now:                    func() time.Time { return now },
		FailedSandboxReapGrace: grace,
	}
	r.RunnerFactory = &PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}
	return reapFixture{r: r, c: c, sess: sess, bundles: []string{gitName, codeName}, runner: RunnerPodName(sess)}
}

func (f reapFixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := f.r.Reconcile(memory.WithSystemApproval(context.Background(), "test"),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}})
	require.NoError(t, err, "Reconcile")
	return res
}

func (f reapFixture) sandboxGone(t *testing.T) bool {
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

func (f reapFixture) sessionPresent(t *testing.T) bool {
	t.Helper()
	err := f.c.Get(memory.WithSystemApproval(context.Background(), "test"), client.ObjectKey{Namespace: "default", Name: "s1"}, &spiceboxv1alpha1.AgentSession{})
	return err == nil
}

// seedWorkspacePVC creates the AgentSession-owned workspace PVC into the fake
// client, mirroring what ensureWorkspacePVC creates during normal
// provisioning. Used to assert reapSessionPods preserves durable storage.
func (f reapFixture) seedWorkspacePVC(t *testing.T) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := BuildWorkspacePVC(f.sess, "rwx-storage", "2Gi")
	require.NoError(t, f.c.Create(memory.WithSystemApproval(context.Background(), "test"), pvc), "seed workspace PVC")
	return pvc
}

// seedDetectorPod creates a content-guard detector/cosidecar pod into the
// fake client, carrying both the agentsession label (shared with every
// session-owned pod) and the sidecartoolbox label — the pair
// cleanupOrphanedSidecarPods' selector uses to find detector/cosidecar pods
// alongside separate-pod sidecars.
func (f reapFixture) seedDetectorPod(t *testing.T) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s1-detector-promptinjection",
			Namespace: "default",
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": f.sess.Name,
				labelSidecarToolbox:                        "promptinjection",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "detector", Image: "detector:dev"}}},
	}
	require.NoError(t, f.c.Create(memory.WithSystemApproval(context.Background(), "test"), pod), "seed detector pod")
	return pod
}

func (f reapFixture) detectorGone(t *testing.T) bool {
	t.Helper()
	err := f.c.Get(memory.WithSystemApproval(context.Background(), "test"),
		client.ObjectKey{Namespace: "default", Name: "s1-detector-promptinjection"}, &corev1.Pod{})
	return apierrors.IsNotFound(err)
}

func (f reapFixture) pvcExists(t *testing.T) bool {
	t.Helper()
	err := f.c.Get(memory.WithSystemApproval(context.Background(), "test"),
		client.ObjectKey{Namespace: "default", Name: podspec.WorkspaceClaimName(f.sess)}, &corev1.PersistentVolumeClaim{})
	return err == nil
}

func TestReconcileReapsFailedSandbox(t *testing.T) {
	t.Run("past grace: bundle SpiceboxSessions + runner pod deleted, AgentSession kept", func(t *testing.T) {
		f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseFailed, time.Minute, -2*time.Minute)
		res := f.reconcile(t)
		assert.Zero(t, res.RequeueAfter, "a reap returns no requeue")
		assert.True(t, f.sandboxGone(t), "bundle SpiceboxSessions + runner pod should be deleted")
		assert.True(t, f.sessionPresent(t), "the AgentSession itself must survive the reap")
	})

	t.Run("within grace: nothing deleted, requeue after the remaining grace", func(t *testing.T) {
		f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseFailed, time.Minute, -20*time.Second)
		res := f.reconcile(t)
		assert.Equal(t, 40*time.Second, res.RequeueAfter, "requeue ≈ grace minus elapsed")
		assert.False(t, f.sandboxGone(t), "within the grace the sandbox is kept for debugging")
		assert.True(t, f.sessionPresent(t))
	})

	t.Run("idempotent: a second reconcile after the reap is a clean no-op", func(t *testing.T) {
		f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseFailed, time.Minute, -2*time.Minute)
		f.reconcile(t)
		require.True(t, f.sandboxGone(t), "first pass reaps")
		// Second pass: bundles + runner already gone (NotFound), so the reap is
		// a no-op that neither errors nor re-provisions; the session stays.
		res := f.reconcile(t)
		assert.Zero(t, res.RequeueAfter)
		assert.True(t, f.sandboxGone(t), "stays reaped (terminal-stable, no re-provision)")
		assert.True(t, f.sessionPresent(t))
	})

	t.Run("disabled (grace 0): a Failed session is never reaped", func(t *testing.T) {
		f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseFailed, 0, -2*time.Minute)
		f.reconcile(t)
		assert.False(t, f.sandboxGone(t), "reaping disabled → sandbox kept")
		assert.True(t, f.sessionPresent(t))
	})

	t.Run("non-terminal (Idle): never reaped", func(t *testing.T) {
		f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseIdle, time.Minute, -2*time.Minute)
		f.reconcile(t)
		assert.False(t, f.sandboxGone(t), "a non-terminal session's sandbox is never reaped")
		assert.True(t, f.sessionPresent(t))
	})
}

// TestReapSessionPodsPreservesPVC pins the generalized reaper's durable-state
// contract directly (white-box, bypassing Reconcile's gating): bundle
// SpiceboxSessions, the runner pod, and detector/cosidecar pods are all torn
// down, while the AgentSession-owned workspace PVC — durable state that must
// survive a reap — is left untouched.
func TestReapSessionPodsPreservesPVC(t *testing.T) {
	f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseFailed, time.Minute, -2*time.Minute)
	f.seedWorkspacePVC(t)
	f.seedDetectorPod(t)

	_, err := f.r.reapSessionPods(memory.WithSystemApproval(context.Background(), "test"), f.sess)
	require.NoError(t, err)

	assert.True(t, f.sandboxGone(t), "bundle SpiceboxSessions + runner pod deleted")
	assert.True(t, f.detectorGone(t), "detector pod deleted")
	assert.True(t, f.pvcExists(t), "AgentSession-owned workspace PVC preserved")
}
