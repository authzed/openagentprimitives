package pod_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

func demoSession() *v1alpha1.SpiceboxSession {
	return &v1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns", UID: "demo-uid"},
		Spec:       v1alpha1.SpiceboxSessionSpec{Class: "demo-class"},
	}
}

func demoClass() v1alpha1.SpiceboxClassSpec {
	return v1alpha1.SpiceboxClassSpec{
		Image: "ghcr.io/demo/spicebox-sandbox:test",
		Resources: v1alpha1.SpiceboxResources{
			CPU:              resource.MustParse("1"),
			Memory:           resource.MustParse("256Mi"),
			EphemeralStorage: resource.MustParse("1Gi"),
		},
	}
}

// newRuntime builds a pod Runtime over a fake client seeded with objs. It
// returns the fake client alongside the Runtime so a test can assert against
// cluster state directly (e.g. "no pod was created") without a production-only
// accessor on Runtime.
func newRuntime(t *testing.T, objs ...runtime.Object) (sandboxkinds.Runtime, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(objs...).Build()
	rt, err := pod.Kind{}.NewRuntime(sandboxkinds.Deps{Client: c})
	require.NoError(t, err, "NewRuntime must succeed with a client")
	return rt, c
}

func TestKind_RegisteredUnderPod(t *testing.T) {
	k, ok := registry.Get("pod")
	require.True(t, ok, "the pod kind must self-register at init()")
	assert.Equal(t, "pod", k.Name())
}

func TestKind_Capabilities(t *testing.T) {
	k := pod.Kind{}
	assert.True(t, k.Supports(sandboxkinds.FeatureSharedWorkspace))
	assert.True(t, k.Supports(sandboxkinds.FeatureConfigMapMounts))
	assert.True(t, k.Supports(sandboxkinds.FeatureToolchainOverlay))
	// The pod kind's own applyMounts already emits an initContainer to
	// expand skill archives, so an unpacking mount rides the same machinery.
	assert.True(t, k.Supports(sandboxkinds.FeatureUnpackMounts))
	// Per-session NetworkPolicies enforce L3/L4 only; hostname-level egress is
	// recorded on status but never enforced, so the pod kind must not claim it.
	assert.False(t, k.Supports(sandboxkinds.FeatureHostEgressAllowlist))
	assert.Equal(t, "kubernetes-pvc", k.WorkspaceDomain())
}

func TestKind_NewRuntimeWithoutClientFailsClosed(t *testing.T) {
	rt, err := pod.Kind{}.NewRuntime(sandboxkinds.Deps{})
	require.Error(t, err, "a pod runtime without a client cannot work")
	// The returned Runtime must be a genuine nil interface, never a typed-nil
	// pointer: a typed nil passes a != nil guard and panics on first call.
	assert.Nil(t, rt)
}

func TestKind_ValidateClassAcceptsAnEmptyImage(t *testing.T) {
	assert.NoError(t, pod.Kind{}.ValidateClass(demoClass()))

	noImage := demoClass()
	noImage.Image = ""
	assert.NoError(t, pod.Kind{}.ValidateClass(noImage),
		"an empty image is filled from the operator default, so it is not a class error")
}

func TestRuntime_EnsureCreatesPodAndReturnsHandle(t *testing.T) {
	rt, _ := newRuntime(t)

	h, err := rt.Ensure(context.Background(), sandboxkinds.EnsureRequest{
		Session: demoSession(), Class: demoClass(),
	})
	require.NoError(t, err)
	assert.Equal(t, "pod", h.Kind)
	assert.Equal(t, "demo-ns/demo-session-pod", h.Ref)
}

// Ensure must converge, not duplicate: the status write that persists the
// handle can fail after Ensure succeeds, so the next reconcile re-enters with
// no handle and must find the pod it already made.
func TestRuntime_EnsureIsIdempotent(t *testing.T) {
	rt, _ := newRuntime(t)
	req := sandboxkinds.EnsureRequest{Session: demoSession(), Class: demoClass()}

	first, err := rt.Ensure(context.Background(), req)
	require.NoError(t, err)
	second, err := rt.Ensure(context.Background(), req)
	require.NoError(t, err, "a second Ensure must succeed, not conflict")
	assert.Equal(t, first, second, "a second Ensure must return the same handle")
}

func TestRuntime_StatusMapsPodPhase(t *testing.T) {
	cases := []struct {
		name  string
		phase corev1.PodPhase
		ready bool
		want  sandboxkinds.Phase
	}{
		{name: "pending pod: Pending", phase: corev1.PodPending, want: sandboxkinds.PhasePending},
		{name: "running but not ready: Pending", phase: corev1.PodRunning, want: sandboxkinds.PhasePending},
		{name: "running and ready: Ready", phase: corev1.PodRunning, ready: true, want: sandboxkinds.PhaseReady},
		{name: "failed pod: Failed", phase: corev1.PodFailed, want: sandboxkinds.PhaseFailed},
		{name: "succeeded pod: Gone", phase: corev1.PodSucceeded, want: sandboxkinds.PhaseGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-session-pod", Namespace: "demo-ns"},
				Status:     corev1.PodStatus{Phase: tc.phase},
			}
			if tc.ready {
				p.Status.Conditions = []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				}
			}
			rt, _ := newRuntime(t, p)

			got, err := rt.Status(context.Background(),
				sandboxkinds.Handle{Kind: "pod", Ref: "demo-ns/demo-session-pod"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Phase)
		})
	}
}

// A sandbox that does not exist is Gone, not an error — a completed or reaped
// session lands here on every reconcile and must not log an error each time.
func TestRuntime_StatusOfMissingPodIsGone(t *testing.T) {
	rt, _ := newRuntime(t)

	got, err := rt.Status(context.Background(),
		sandboxkinds.Handle{Kind: "pod", Ref: "demo-ns/absent-pod"})
	require.NoError(t, err, "a missing sandbox must not be an error")
	assert.Equal(t, sandboxkinds.PhaseGone, got.Phase)
}

// A pod that is Running but already terminating must NOT report Ready: the
// object outlives the delete (it lingers until the kubelet confirms, and
// forever under envtest, which runs none), so reading only Status.Phase would
// dispatch a tool call into a sandbox that is shutting down.
func TestRuntime_StatusOfTerminatingPodIsGone(t *testing.T) {
	now := metav1.Now()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-session-pod", Namespace: "demo-ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{"demo.invalid/keep"}, // fake client requires one to retain a deleted object
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	rt, _ := newRuntime(t, p)

	got, err := rt.Status(context.Background(),
		sandboxkinds.Handle{Kind: "pod", Ref: "demo-ns/demo-session-pod"})
	require.NoError(t, err)
	assert.Equal(t, sandboxkinds.PhaseGone, got.Phase,
		"a terminating pod must not be reported Ready")
}

func TestRuntime_RejectsForeignHandle(t *testing.T) {
	rt, _ := newRuntime(t)

	_, err := rt.Status(context.Background(),
		sandboxkinds.Handle{Kind: "some-other-kind", Ref: "demo-ns/demo-session-pod"})
	assert.Error(t, err, "a handle owned by another kind must be refused, not interpreted")
}

func TestRuntime_TeardownIsIdempotent(t *testing.T) {
	rt, _ := newRuntime(t)
	h, err := rt.Ensure(context.Background(),
		sandboxkinds.EnsureRequest{Session: demoSession(), Class: demoClass()})
	require.NoError(t, err)

	require.NoError(t, rt.Teardown(context.Background(), h))
	assert.NoError(t, rt.Teardown(context.Background(), h),
		"tearing down an already-gone sandbox must succeed")
}

func TestRuntime_WatchesContributesPod(t *testing.T) {
	rt, _ := newRuntime(t)

	w := rt.Watches()
	require.Len(t, w, 1, "the pod kind watches exactly the pods it owns")
	assert.IsType(t, &corev1.Pod{}, w[0].Object)
}

// Ensure must not create a pod whose workspace claim does not exist yet.
// Creating it anyway yields FailedScheduling, which under restartPolicy:Never
// becomes a terminal pod failure and surfaces as a spurious BundleFailed.
func TestRuntime_EnsureWaitsForAbsentWorkspaceClaim(t *testing.T) {
	sess := demoSession()
	sess.Spec.Workspace = v1alpha1.WorkspaceConfig{
		Mode:            v1alpha1.WorkspaceShared,
		SharedClaimName: "demo-session-workspace",
	}
	rt, c := newRuntime(t) // no PVC in the fake client

	_, err := rt.Ensure(context.Background(),
		sandboxkinds.EnsureRequest{Session: sess, Class: demoClass()})

	require.Error(t, err)
	assert.True(t, errors.Is(err, sandboxkinds.ErrPreconditionPending),
		"an absent claim is a wait, not a failure")
	assert.Contains(t, err.Error(), "demo-session-workspace",
		"the error must name what is being waited on")

	var pods corev1.PodList
	require.NoError(t, c.List(context.Background(), &pods))
	assert.Empty(t, pods.Items, "no pod may be created while the claim is absent")
}

// Once the claim exists the pod is created normally.
func TestRuntime_EnsureProceedsOnceClaimExists(t *testing.T) {
	sess := demoSession()
	sess.Spec.Workspace = v1alpha1.WorkspaceConfig{
		Mode:            v1alpha1.WorkspaceShared,
		SharedClaimName: "demo-session-workspace",
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session-workspace", Namespace: "demo-ns"},
	}
	rt, _ := newRuntime(t, claim)

	h, err := rt.Ensure(context.Background(),
		sandboxkinds.EnsureRequest{Session: sess, Class: demoClass()})
	require.NoError(t, err)
	assert.Equal(t, "demo-ns/demo-session-pod", h.Ref)
}

// An existing-but-unbound claim must NOT block pod creation: the workspace
// StorageClass is WaitForFirstConsumer, so the pod is its claim's first
// consumer and is what triggers binding. Waiting for Bound here would
// deadlock — nothing else would ever create that first consumer.
func TestRuntime_EnsureProceedsWithUnboundClaim(t *testing.T) {
	sess := demoSession()
	sess.Spec.Workspace = v1alpha1.WorkspaceConfig{
		Mode:            v1alpha1.WorkspaceShared,
		SharedClaimName: "demo-session-workspace",
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session-workspace", Namespace: "demo-ns"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	rt, _ := newRuntime(t, claim)

	h, err := rt.Ensure(context.Background(),
		sandboxkinds.EnsureRequest{Session: sess, Class: demoClass()})
	require.NoError(t, err, "an unbound (Pending) claim must not block pod creation")
	assert.Equal(t, "demo-ns/demo-session-pod", h.Ref)
}

// An isolated-workspace session has no claim to wait for.
func TestRuntime_EnsureDoesNotWaitWhenWorkspaceIsIsolated(t *testing.T) {
	sess := demoSession()
	sess.Spec.Workspace = v1alpha1.WorkspaceConfig{Mode: v1alpha1.WorkspaceIsolated}
	rt, _ := newRuntime(t)

	_, err := rt.Ensure(context.Background(),
		sandboxkinds.EnsureRequest{Session: sess, Class: demoClass()})
	assert.NoError(t, err, "an isolated workspace mounts no claim")
}

// Status reports the shared vocabulary so a condition reads the same whichever
// backend produced it. The specific cause stays in Message.
func TestRuntime_StatusUsesStandardReasons(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*corev1.Pod)
		wantPhase  sandboxkinds.Phase
		wantReason string
	}{
		{
			name:       "running and ready: Ready/Ready",
			mutate:     func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning; setPodReady(p, true) },
			wantPhase:  sandboxkinds.PhaseReady,
			wantReason: sandboxkinds.ReasonReady,
		},
		{
			name:       "running but not ready: Pending/NotReady",
			mutate:     func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
			wantPhase:  sandboxkinds.PhasePending,
			wantReason: sandboxkinds.ReasonNotReady,
		},
		{
			name:       "pending: Pending/Creating",
			mutate:     func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending },
			wantPhase:  sandboxkinds.PhasePending,
			wantReason: sandboxkinds.ReasonCreating,
		},
		{
			name: "OOM killed: Failed/OOMKilled",
			mutate: func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
				}}
			},
			wantPhase:  sandboxkinds.PhaseFailed,
			wantReason: sandboxkinds.ReasonOOMKilled,
		},
		{
			name:       "failed without OOM: Failed/Crashed",
			mutate:     func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
			wantPhase:  sandboxkinds.PhaseFailed,
			wantReason: sandboxkinds.ReasonCrashed,
		},
		{
			name:       "succeeded: Gone/Gone",
			mutate:     func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
			wantPhase:  sandboxkinds.PhaseGone,
			wantReason: sandboxkinds.ReasonGone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-session-pod", Namespace: "demo-ns"}}
			tc.mutate(p)
			rt, _ := newRuntime(t, p)

			got, err := rt.Status(context.Background(),
				sandboxkinds.Handle{Kind: "pod", Ref: "demo-ns/demo-session-pod"})
			require.NoError(t, err)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, tc.wantReason, got.Reason)
		})
	}
}

// A pod wedged in a terminal waiting state never reaches the Failed phase — the
// kubelet retries forever — so phase alone misses it. The standard reason says
// StartFailed; the specific cause must survive in Message, because that is what
// reaches the user's channel.
func TestRuntime_StatusTerminalWaitIsStartFailedWithCauseInMessage(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session-pod", Namespace: "demo-ns"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ImagePullBackOff", Message: "image not found",
				}},
			}},
		},
	}
	rt, _ := newRuntime(t, p)

	got, err := rt.Status(context.Background(),
		sandboxkinds.Handle{Kind: "pod", Ref: "demo-ns/demo-session-pod"})
	require.NoError(t, err)
	assert.Equal(t, sandboxkinds.PhaseFailed, got.Phase)
	assert.Equal(t, sandboxkinds.ReasonStartFailed, got.Reason)
	assert.Contains(t, got.Message, "ImagePullBackOff",
		"the specific cause must not be flattened away")
}

func setPodReady(p *corev1.Pod, ready bool) {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
}
