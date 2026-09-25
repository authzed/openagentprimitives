package cloud

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// ---- IsGKEAutopilot tests ----

func TestIsGKEAutopilot(t *testing.T) {
	const wardenWebhook = "warden-validating.common-webhooks.networking.gke.io"
	cases := []struct {
		name      string
		objs      []runtime.Object
		wantFound bool
	}{
		{
			name: "Warden webhook present → Autopilot detected",
			objs: []runtime.Object{
				&admissionregistrationv1.ValidatingWebhookConfiguration{
					ObjectMeta: metav1.ObjectMeta{Name: wardenWebhook},
				},
			},
			wantFound: true,
		},
		{
			name:      "no Warden webhook → not Autopilot",
			objs:      nil,
			wantFound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)
			got, err := IsGKEAutopilot(context.Background(), kc)
			require.NoError(t, err)
			assert.Equal(t, tc.wantFound, got)
		})
	}
}

// ---- latestProvisioningFailure tests ----

func TestLatestProvisioningFailure(t *testing.T) {
	ns := workspaceProbeNamespace
	pvcName := "ap-rwx-probe-testpvc"

	cases := []struct {
		name       string
		events     []runtime.Object
		wantReason string
	}{
		{
			name:       "no events → empty reason",
			events:     nil,
			wantReason: "",
		},
		{
			name: "single ProvisioningFailed warning → returns message",
			events: []runtime.Object{
				&corev1.Event{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "ev1",
						Namespace: ns,
					},
					InvolvedObject: corev1.ObjectReference{
						Name:      pvcName,
						Namespace: ns,
					},
					Type:          corev1.EventTypeWarning,
					Reason:        "ProvisioningFailed",
					Message:       "denied by autogke-no-write-mode-hostpath",
					LastTimestamp: metav1.Time{Time: time.Now().Add(-5 * time.Second)},
				},
			},
			wantReason: "denied by autogke-no-write-mode-hostpath",
		},
		{
			name: "multiple warnings → returns most recent message",
			events: []runtime.Object{
				&corev1.Event{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "ev-old",
						Namespace: ns,
					},
					InvolvedObject: corev1.ObjectReference{
						Name:      pvcName,
						Namespace: ns,
					},
					Type:          corev1.EventTypeWarning,
					Reason:        "ProvisioningFailed",
					Message:       "old failure reason",
					LastTimestamp: metav1.Time{Time: time.Now().Add(-30 * time.Second)},
				},
				&corev1.Event{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "ev-recent",
						Namespace: ns,
					},
					InvolvedObject: corev1.ObjectReference{
						Name:      pvcName,
						Namespace: ns,
					},
					Type:          corev1.EventTypeWarning,
					Reason:        "ProvisioningFailed",
					Message:       "most recent failure reason",
					LastTimestamp: metav1.Time{Time: time.Now().Add(-2 * time.Second)},
				},
			},
			wantReason: "most recent failure reason",
		},
		{
			name: "only Normal events → empty reason",
			events: []runtime.Object{
				&corev1.Event{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "ev-normal",
						Namespace: ns,
					},
					InvolvedObject: corev1.ObjectReference{
						Name:      pvcName,
						Namespace: ns,
					},
					Type:          corev1.EventTypeNormal,
					Reason:        "Provisioning",
					Message:       "provisioning started",
					LastTimestamp: metav1.Time{Time: time.Now().Add(-1 * time.Second)},
				},
			},
			wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.events...)
			got, err := latestProvisioningFailure(context.Background(), kc, ns, pvcName)
			require.NoError(t, err)
			assert.Equal(t, tc.wantReason, got)
		})
	}
}

// ---- NewProvisioningProbe (PVC-Bound) tests ----

func TestNewProvisioningProbe_PVCBound_BindsAndCleansUp(t *testing.T) {
	kc := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workspaceProbeNamespace}},
	)
	// Simulate the provisioner binding the PVC via a create reactor that
	// spawns a goroutine to patch status.
	kc.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pvc := a.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
		go func() {
			time.Sleep(50 * time.Millisecond)
			bound := pvc.DeepCopy()
			bound.Status.Phase = corev1.ClaimBound
			_, _ = kc.CoreV1().PersistentVolumeClaims(pvc.Namespace).UpdateStatus(
				context.Background(), bound, metav1.UpdateOptions{},
			)
		}()
		return false, nil, nil // let default reactor create it
	})

	step, cleanup, err := NewProvisioningProbe(context.Background(), kc, "standard", ProbeOptions{})
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	bound, why, perr := step(context.Background())
	require.NoError(t, perr)
	assert.True(t, bound)
	assert.Empty(t, why, "a bound PVC has nothing to explain")

	// cleanup must remove both probe resources so a failed install leaves no
	// PVC holding a volume.
	cleanup()
	pvcList, err := kc.CoreV1().PersistentVolumeClaims(workspaceProbeNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pvcList.Items, "probe PVC should be deleted by cleanup")

	podList, err := kc.CoreV1().Pods(workspaceProbeNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, podList.Items, "probe Pod should be deleted by cleanup")
}

func TestNewProvisioningProbe_PVCBound_PollGetError_ReturnsError(t *testing.T) {
	kc := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workspaceProbeNamespace}},
	)
	// After PVC + Pod are created, inject a Get failure on the PVC so the step
	// hits the hard-error path rather than reporting a transient reason.
	pvcCreated := false
	podCreated := false
	kc.PrependReactor("get", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if pvcCreated && podCreated {
			return true, nil, k8serrors.NewInternalError(errors.New("injected API error"))
		}
		return false, nil, nil
	})
	kc.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pvcCreated = true
		return false, nil, nil // let default reactor create it
	})
	kc.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		podCreated = true
		return false, nil, nil // let default reactor create it
	})

	step, cleanup, err := NewProvisioningProbe(context.Background(), kc, "standard", ProbeOptions{})
	require.NoError(t, err)
	defer cleanup()

	_, _, perr := step(context.Background())
	require.Error(t, perr, "Get failure must surface as an error, not a transient reason")
	assert.Contains(t, perr.Error(), "poll probe PVC")
	var statusErr *k8serrors.StatusError
	require.ErrorAs(t, perr, &statusErr)
	assert.Equal(t, int32(500), statusErr.ErrStatus.Code)
}

// ---- podNotReadyDetail tests ----

func TestPodNotReadyDetail(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "unschedulable condition wins (most actionable)",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Message: "0/9 nodes are available: Insufficient cpu",
				}},
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
				}},
			}},
			want: "unschedulable: 0/9 nodes are available: Insufficient cpu",
		},
		{
			name: "container waiting reason + message",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "not found"}},
			}}}},
			want: "ImagePullBackOff: not found",
		},
		{
			name: "container waiting reason only",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
			}}}},
			want: "ContainerCreating",
		},
		{
			name: "phase fallback when no condition/container detail",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			want: "phase Pending",
		},
		{
			name: "empty status yields empty detail",
			pod:  &corev1.Pod{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, podNotReadyDetail(tc.pod))
		})
	}
}

// ---- NewProvisioningProbe WaitForPodRunning (RWO attach) tests ----

func TestNewProvisioningProbe_PodRunning_ReadyOnlyWhenPodRuns(t *testing.T) {
	kc := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workspaceProbeNamespace}},
	)
	// Bind the PVC immediately, but leave the pod Pending — this is exactly the
	// FailedAttachVolume shape: PVC Bound, volume never attached.
	kc.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pvc := a.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
		go func() {
			time.Sleep(20 * time.Millisecond)
			bound := pvc.DeepCopy()
			bound.Status.Phase = corev1.ClaimBound
			_, _ = kc.CoreV1().PersistentVolumeClaims(pvc.Namespace).UpdateStatus(context.Background(), bound, metav1.UpdateOptions{})
		}()
		return false, nil, nil
	})

	step, cleanup, err := NewProvisioningProbe(context.Background(), kc, "hd", ProbeOptions{
		AccessMode:        corev1.ReadWriteOnce,
		WaitForPodRunning: true,
	})
	require.NoError(t, err)
	defer cleanup()

	// PVC binds but pod is Pending → NOT ready (this is the gap the bound-only
	// probe missed).
	time.Sleep(60 * time.Millisecond)
	ready, _, perr := step(context.Background())
	require.NoError(t, perr)
	assert.False(t, ready, "PVC bound but pod not Running must report not-ready")

	// Flip the pod to Running → ready.
	pods, err := kc.CoreV1().Pods(workspaceProbeNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1)
	running := pods.Items[0].DeepCopy()
	running.Status.Phase = corev1.PodRunning
	_, err = kc.CoreV1().Pods(workspaceProbeNamespace).UpdateStatus(context.Background(), running, metav1.UpdateOptions{})
	require.NoError(t, err)

	ready, _, perr = step(context.Background())
	require.NoError(t, perr)
	assert.True(t, ready, "pod Running ⇒ volume attached ⇒ ready")
}

// TestNewProvisioningProbe_PVCBound_UnboundWithNoEvent_ReportsWhy is the
// no-silent-errors guard on the SHIPPED workspace path. awaitProvisioningProbe
// turns a non-empty transientReason into the only diagnosis the installer ever
// prints and drops an empty one on the floor, so a probe that reports "" while
// the PVC sits Pending tells the user nothing at all about why shared-workspace
// storage will not provision. The consumer pod is where the real cause shows up
// (unschedulable, stuck ContainerCreating), exactly as on the WaitForPodRunning
// path.
func TestNewProvisioningProbe_PVCBound_UnboundWithNoEvent_ReportsWhy(t *testing.T) {
	kc := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workspaceProbeNamespace}},
	)
	// The consumer pod cannot schedule; no PVC ProvisioningFailed event is ever
	// recorded (WaitForFirstConsumer binding never starts).
	kc.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodPending
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Message: "0/1 nodes are available: Insufficient cpu",
		}}
		return false, nil, nil
	})

	step, cleanup, err := NewProvisioningProbe(context.Background(), kc, "standard", ProbeOptions{})
	require.NoError(t, err)
	defer cleanup()

	ready, why, perr := step(context.Background())
	require.NoError(t, perr)
	assert.False(t, ready, "an unbound PVC is not ready")
	assert.NotEmpty(t, why, "an unbound PVC with no event must still report WHY, not an empty reason")
	assert.Contains(t, why, "Pending", "reason should name the PVC's unbound phase")
	assert.Contains(t, why, "Insufficient cpu", "reason should carry the consumer pod's actual blocker")
}

// TestNewProvisioningProbe_PVCBound_EventWins asserts the preference order is
// unchanged: a real ProvisioningFailed event beats the synthesized fallback.
func TestNewProvisioningProbe_PVCBound_EventWins(t *testing.T) {
	kc := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workspaceProbeNamespace}},
	)
	step, cleanup, err := NewProvisioningProbe(context.Background(), kc, "standard", ProbeOptions{})
	require.NoError(t, err)
	defer cleanup()

	// Record the ProvisioningFailed Warning against the probe PVC the probe
	// just created. (Doing this from inside a create reactor deadlocks the
	// fake clientset — the reactor already holds its lock.)
	pvcs, err := kc.CoreV1().PersistentVolumeClaims(workspaceProbeNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pvcs.Items, 1)
	_, err = kc.CoreV1().Events(workspaceProbeNamespace).Create(context.Background(), &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: pvcs.Items[0].Name + ".evt", Namespace: workspaceProbeNamespace},
		InvolvedObject: corev1.ObjectReference{Name: pvcs.Items[0].Name, Namespace: workspaceProbeNamespace},
		Type:           corev1.EventTypeWarning,
		Reason:         "ProvisioningFailed",
		Message:        "hostPath denied by Autopilot",
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	ready, why, perr := step(context.Background())
	require.NoError(t, perr)
	assert.False(t, ready)
	assert.Equal(t, "hostPath denied by Autopilot", why, "a real event must win over the synthesized fallback")
}

// ---- ResolveRWXOrBundled tests ----

// The decision every cloud but GKE makes. Its two arms differ in exactly one
// thing that matters downstream: NeedsBundled, which tells oap install to apply
// the local-path provisioner manifests before probing.
func TestResolveRWXOrBundled(t *testing.T) {
	cases := []struct {
		name      string
		objs      []runtime.Object
		preferred []string
		want      Decision
	}{
		{
			name:      "cloud-native RWX class present: use it, probe before use, no bundled provisioner",
			objs:      []runtime.Object{storageClassObj("efs-sc", "efs.csi.aws.com")},
			preferred: []string{"efs.csi.aws.com"},
			want:      Decision{ClassName: "efs-sc", Verify: WorkspaceProbeBeforeUse},
		},
		{
			name:      "no RWX class at all: fall back to the bundled class with NeedsBundled",
			objs:      nil,
			preferred: []string{"efs.csi.aws.com"},
			want:      Decision{ClassName: BundledWorkspaceStorageClass, NeedsBundled: true, Verify: WorkspaceProbeBeforeUse},
		},
		{
			name:      "no preferred driver (local/unmanaged): a generic known RWX class still wins",
			objs:      []runtime.Object{storageClassObj("nfs-sc", "nfs.csi.k8s.io")},
			preferred: nil,
			want:      Decision{ClassName: "nfs-sc", Verify: WorkspaceProbeBeforeUse},
		},
		{
			name:      "unknown provisioner is not trusted as RWX: fall back to bundled",
			objs:      []runtime.Object{storageClassObj("custom-sc", "custom.provisioner.io")},
			preferred: nil,
			want:      Decision{ClassName: BundledWorkspaceStorageClass, NeedsBundled: true, Verify: WorkspaceProbeBeforeUse},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)
			got, err := ResolveRWXOrBundled(context.Background(), kc, tc.preferred)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---- FindRWXClass tests ----

func storageClassObj(name, provisioner string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: provisioner,
	}
}

func TestFindRWXClass(t *testing.T) {
	cases := []struct {
		name      string
		objs      []runtime.Object
		preferred []string
		wantClass string
	}{
		{
			name:      "no storage classes → empty",
			objs:      nil,
			preferred: nil,
			wantClass: "",
		},
		{
			name: "preferred provisioner matches → returns that class",
			objs: []runtime.Object{
				storageClassObj("efs-sc", "efs.csi.aws.com"),
				storageClassObj("nfs-sc", "nfs.csi.k8s.io"),
			},
			preferred: []string{"efs.csi.aws.com"},
			wantClass: "efs-sc",
		},
		{
			name: "no preferred match, known provisioner → fallback to known",
			objs: []runtime.Object{
				storageClassObj("nfs-sc", "nfs.csi.k8s.io"),
			},
			preferred: []string{"efs.csi.aws.com"},
			wantClass: "nfs-sc",
		},
		{
			name: "unknown provisioner only → empty",
			objs: []runtime.Object{
				storageClassObj("custom-sc", "custom.provisioner.io"),
			},
			preferred: nil,
			wantClass: "",
		},
		{
			name: "preferred beats known even if known listed first",
			objs: []runtime.Object{
				storageClassObj("nfs-sc", "nfs.csi.k8s.io"),
				storageClassObj("filestore-sc", "filestore.csi.storage.gke.io"),
			},
			preferred: []string{"filestore.csi.storage.gke.io"},
			wantClass: "filestore-sc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)
			got, err := FindRWXClass(context.Background(), kc, tc.preferred)
			require.NoError(t, err)
			assert.Equal(t, tc.wantClass, got)
		})
	}
}
