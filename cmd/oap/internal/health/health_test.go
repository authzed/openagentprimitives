package health

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

func dep(name string, replicas, ready, updated, unavailable int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready, UpdatedReplicas: updated, UnavailableReplicas: unavailable},
	}
}

func sts(name string, replicas, ready, updated int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.StatefulSetStatus{ReadyReplicas: ready, UpdatedReplicas: updated},
	}
}

func waitingPod(name, app, reason string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": app}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}},
		}},
	}
}

func TestWorkloadCheck(t *testing.T) {
	cases := []struct {
		name         string
		comp         Component
		objs         []runtime.Object
		wantStatus   Status
		wantContains string
		wantNotFound bool
	}{
		{
			name:       "healthy deployment: OK, N/N ready",
			comp:       Deployment("spicebox-spicedb", false),
			objs:       []runtime.Object{dep("spicebox-spicedb", 1, 1, 1, 0)},
			wantStatus: OK, wantContains: "1/1 ready",
		},
		{
			name: "crashlooping required deployment: Failed, names the reason",
			comp: Deployment("agentprimitives-authzd", false),
			objs: []runtime.Object{
				dep("agentprimitives-authzd", 1, 0, 1, 1),
				waitingPod("authzd-x", "agentprimitives-authzd", "CrashLoopBackOff"),
			},
			wantStatus: Failed, wantContains: "CrashLoopBackOff",
		},
		{
			name:       "stuck rollout (unavailable>0, old replica Ready): Failed not OK",
			comp:       Deployment("spicebox-operator", false),
			objs:       []runtime.Object{dep("spicebox-operator", 1, 1, 1, 1)},
			wantStatus: Failed, wantContains: "1/1 ready",
		},
		{
			name:       "optional deployment not ready: Optional, not Failed",
			comp:       Deployment("spicebox-graphiti", true),
			objs:       []runtime.Object{dep("spicebox-graphiti", 1, 0, 0, 1)},
			wantStatus: Optional, wantContains: "0/1 ready",
		},
		{
			name:       "missing required deployment: Failed",
			comp:       Deployment("agentprimitives-authzd", false),
			objs:       nil,
			wantStatus: Failed, wantContains: "not found", wantNotFound: true,
		},
		{
			name:       "healthy statefulset: OK",
			comp:       StatefulSet("spicebox-nats", false),
			objs:       []runtime.Object{sts("spicebox-nats", 1, 1, 1)},
			wantStatus: OK, wantContains: "1/1 ready",
		},
		{
			name:       "missing required statefulset: Failed and NotFound",
			comp:       StatefulSet("spicebox-nats", false),
			objs:       nil,
			wantStatus: Failed, wantContains: "not found", wantNotFound: true,
		},
		{
			name:       "present-but-unready required deployment: Failed and NOT NotFound",
			comp:       Deployment("spicebox-operator", false),
			objs:       []runtime.Object{dep("spicebox-operator", 1, 0, 1, 1)},
			wantStatus: Failed, wantContains: "0/1 ready", wantNotFound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &kube.Bundle{Typed: fake.NewSimpleClientset(tc.objs...)}
			res := tc.comp.Check(context.Background(), b)
			assert.Equal(t, tc.wantStatus, res.Status, "status")
			assert.Contains(t, res.Detail, tc.wantContains, "detail")
			assert.Equal(t, tc.wantNotFound, res.NotFound, "NotFound")
		})
	}
}

func TestWorkloadRepair_StampsRestartAnnotation(t *testing.T) {
	clientset := fake.NewSimpleClientset(dep("spicebox-operator", 1, 0, 1, 1))
	b := &kube.Bundle{Typed: clientset}

	r, ok := Deployment("spicebox-operator", false).(Repairer)
	require.True(t, ok, "workload must implement Repairer")
	require.NoError(t, r.Repair(context.Background(), b))

	got, err := clientset.AppsV1().Deployments(namespace).Get(context.Background(), "spicebox-operator", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, got.Spec.Template.Annotations["ap.authzed.com/restartedAt"], "rollout-restart annotation must be set")
}
