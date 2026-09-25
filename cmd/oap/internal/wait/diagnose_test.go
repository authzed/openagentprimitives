package wait

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const diagNS = "agentprimitives-system"

// seedWorkload returns a fake clientset with a Deployment whose selector matches
// one not-ready pod, an optional PVC, and optional Warning events on that pod.
func seedWorkload(t *testing.T, podPhase corev1.PodPhase, waitReason string, pvc *corev1.PersistentVolumeClaim, events ...corev1.Event) *fake.Clientset {
	t.Helper()
	sel := map[string]string{"app.kubernetes.io/name": "spicebox-postgres"}
	objs := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "spicebox-postgres", Namespace: diagNS},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: sel}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "spicebox-postgres-abc", Namespace: diagNS, Labels: sel},
			Spec: corev1.PodSpec{Volumes: func() []corev1.Volume {
				if pvc == nil {
					return nil
				}
				return []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}}}}
			}()},
			Status: corev1.PodStatus{
				Phase:      podPhase,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitReason}}}},
			},
		},
	}
	if pvc != nil {
		objs = append(objs, pvc)
	}
	for i := range events {
		e := events[i]
		objs = append(objs, &e)
	}
	return fake.NewSimpleClientset(objs...)
}

// seedStatefulSetWorkload returns a fake clientset with a StatefulSet whose
// selector matches one not-ready pod and optional Warning events.
func seedStatefulSetWorkload(t *testing.T, podPhase corev1.PodPhase, waitReason string, events ...corev1.Event) *fake.Clientset {
	t.Helper()
	sel := map[string]string{"app.kubernetes.io/name": "spicebox-nats"}
	objs := []runtime.Object{
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "spicebox-nats", Namespace: diagNS},
			Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: sel}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "spicebox-nats-0", Namespace: diagNS, Labels: sel},
			Status: corev1.PodStatus{
				Phase:      podPhase,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitReason}}}},
			},
		},
	}
	for i := range events {
		e := events[i]
		objs = append(objs, &e)
	}
	return fake.NewSimpleClientset(objs...)
}

// NOTE: the fake clientset ignores field selectors on Events.List, so
// warningEvents() must also filter by InvolvedObject.Name in code — the seeded
// events below are all for the target pod so the filter is exercised, not bypassed.
func warnEvent(idx int, reason, msg string, count int32, ageSec int, podName string) corev1.Event {
	return corev1.Event{
		// Give each event a unique name so same-reason events don't collide in
		// the fake object tracker.
		ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", reason, idx), Namespace: diagNS},
		InvolvedObject: corev1.ObjectReference{Name: podName, Namespace: diagNS, Kind: "Pod"},
		Reason:         reason,
		Message:        msg,
		Type:           corev1.EventTypeWarning,
		Count:          count,
		LastTimestamp:  metav1.NewTime(time.Now().Add(-time.Duration(ageSec) * time.Second)),
	}
}

func TestDiagnoseDeployment_SurfacesPodPhaseEventsAndPVC(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-postgres-data", Namespace: diagNS},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	cs := seedWorkload(t, corev1.PodPending, "ContainerCreating", pvc,
		warnEvent(0, "FailedAttachVolume", "pd-balanced disk type cannot be used by n4-standard-4 machine type", 12, 1, "spicebox-postgres-abc"))

	d, err := DiagnoseDeployment(context.Background(), cs, diagNS, "spicebox-postgres")
	require.NoError(t, err)
	assert.Equal(t, "deployment/spicebox-postgres", d.Workload)
	assert.Equal(t, "spicebox-postgres-abc", d.Pod)
	assert.Equal(t, "Pending", d.PodPhase)
	assert.Equal(t, "ContainerCreating", d.Reason)
	require.Len(t, d.Events, 1)
	assert.Equal(t, "FailedAttachVolume", d.Events[0].Reason)
	assert.Equal(t, int32(12), d.Events[0].Count)
	require.Len(t, d.PVCs, 1)
	assert.Equal(t, "spicebox-postgres-data", d.PVCs[0].Name)
	assert.Equal(t, "Bound", d.PVCs[0].Phase)
}

func TestDiagnoseDeployment_CapsEventsAtThree(t *testing.T) {
	cs := seedWorkload(t, corev1.PodPending, "CrashLoopBackOff", nil,
		warnEvent(0, "BackOff", "back-off restarting", 5, 1, "spicebox-postgres-abc"),
		warnEvent(1, "Unhealthy", "readiness probe failed", 4, 2, "spicebox-postgres-abc"),
		warnEvent(2, "FailedMount", "timed out waiting", 3, 3, "spicebox-postgres-abc"),
		warnEvent(3, "Old", "should be dropped (4th, oldest)", 1, 99, "spicebox-postgres-abc"))
	d, err := DiagnoseDeployment(context.Background(), cs, diagNS, "spicebox-postgres")
	require.NoError(t, err)
	assert.Len(t, d.Events, 3, "events are capped at 3, newest first")
	assert.Equal(t, "BackOff", d.Events[0].Reason, "newest event first")
}

func TestDiagnoseDeployment_ForeignPodEventRejected(t *testing.T) {
	// Seed one event for the target pod and one for a different pod.
	// warningEvents() must filter by InvolvedObject.Name so the foreign event
	// does not appear in the Diagnosis (the fake clientset ignores field selectors,
	// so the in-code filter is the only guard).
	cs := seedWorkload(t, corev1.PodPending, "CrashLoopBackOff", nil,
		warnEvent(0, "BackOff", "back-off restarting", 3, 1, "spicebox-postgres-abc"),
		warnEvent(1, "BackOff", "back-off restarting", 7, 1, "some-other-pod"))
	d, err := DiagnoseDeployment(context.Background(), cs, diagNS, "spicebox-postgres")
	require.NoError(t, err)
	require.Len(t, d.Events, 1, "foreign-pod event must be rejected")
	assert.Equal(t, "BackOff", d.Events[0].Reason)
}

func TestDiagnoseDeployment_MissingDeployment_ReturnsError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	_, err := DiagnoseDeployment(context.Background(), cs, diagNS, "nope")
	require.Error(t, err)
}

func TestDiagnoseStatefulSet_SurfacesPodPhaseAndEvents(t *testing.T) {
	cs := seedStatefulSetWorkload(t, corev1.PodPending, "ContainerCreating",
		warnEvent(0, "FailedScheduling", "0/3 nodes are available: Insufficient memory", 2, 5, "spicebox-nats-0"))

	d, err := DiagnoseStatefulSet(context.Background(), cs, diagNS, "spicebox-nats")
	require.NoError(t, err)
	assert.Equal(t, "statefulset/spicebox-nats", d.Workload)
	assert.Equal(t, "spicebox-nats-0", d.Pod)
	assert.Equal(t, "Pending", d.PodPhase)
	assert.Equal(t, "ContainerCreating", d.Reason)
	require.Len(t, d.Events, 1)
	assert.Equal(t, "FailedScheduling", d.Events[0].Reason)
}
