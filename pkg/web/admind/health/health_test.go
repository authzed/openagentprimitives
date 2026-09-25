package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	// A fresh scheme (not the client-go global) so registering the metrics group
	// stays local to this package's tests. client-go covers every built-in group
	// the snapshot reads (apps/v1 + core/v1); metrics.k8s.io/v1beta1 adds the
	// PodMetrics types the CPU/Memory rollup lists.
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, metricsv1beta1.AddToScheme(s))
	return s
}

func appLabels(name string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": name}
}

func deployment(name string, replicas, available, ready int32) *appsv1.Deployment {
	r := replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Labels: appLabels(name)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &r,
			Selector: &metav1.LabelSelector{MatchLabels: appLabels(name)},
		},
		Status: appsv1.DeploymentStatus{
			Replicas:          replicas,
			AvailableReplicas: available,
			ReadyReplicas:     ready,
			UpdatedReplicas:   ready,
		},
	}
}

func statefulset(name string, replicas, ready int32) *appsv1.StatefulSet {
	r := replicas
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Labels: appLabels(name)},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &r,
			Selector: &metav1.LabelSelector{MatchLabels: appLabels(name)},
		},
		Status: appsv1.StatefulSetStatus{
			Replicas:        replicas,
			ReadyReplicas:   ready,
			UpdatedReplicas: ready,
		},
	}
}

// pod builds a pod owned (by label) by the named workload, optionally Ready,
// with a container restart count and an optional waiting reason (CrashLoopBackOff).
func pod(podName, workload string, ready bool, restarts int32, waitingReason string) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	cs := corev1.ContainerStatus{Name: "main", RestartCount: restarts}
	if waitingReason != "" {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingReason}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: Namespace, Labels: appLabels(workload)},
		Status: corev1.PodStatus{
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
			ContainerStatuses: []corev1.ContainerStatus{cs},
		},
	}
}

func newChecker(t *testing.T, graphitiURL string, objs ...client.Object) *Checker {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	return New(c, graphitiURL, logr.Discard())
}

func componentStatus(t *testing.T, rep Report, name string) Component {
	t.Helper()
	for _, c := range rep.Components {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("component %q not present in report", name)
	return Component{}
}

func TestSnapshot_PerComponentStatus(t *testing.T) {
	objs := []client.Object{
		deployment("spicebox-operator", 2, 2, 2),        // all ready -> Healthy
		deployment("spicebox-channelsd", 2, 1, 1),       // 1/2 -> Degraded
		deployment("spicebox-spicedb-spicedb", 1, 0, 0), // operator-named Deployment; 0/1 -> Down
		deployment("spicebox-webd", 1, 1, 1),            // Healthy
		statefulset("spicebox-nats", 1, 1),              // StatefulSet, ready -> Healthy
		// agentprimitives-authzd + spicebox-postgres deliberately absent -> Down
	}
	chk := newChecker(t, "", objs...)

	rep, err := chk.Snapshot(context.Background())
	require.NoError(t, err, "Snapshot must not error on a well-formed cluster")

	cases := []struct {
		component string
		want      Status
	}{
		{"operator", StatusHealthy},
		{"channelsd", StatusDegraded},
		{"spicedb", StatusDown}, // present but 0 available
		{"webd", StatusHealthy},
		{"nats", StatusHealthy},
		{"authzd", StatusDown},   // absent
		{"postgres", StatusDown}, // absent
		{"graphiti", StatusNotConfigured},
	}
	for _, tc := range cases {
		t.Run(tc.component+" -> "+string(tc.want), func(t *testing.T) {
			assert.Equal(t, tc.want, componentStatus(t, rep, tc.component).Status)
		})
	}
}

func TestSnapshot_DegradedOnCrashLoopDespiteReady(t *testing.T) {
	// All replicas available, but a matched pod is CrashLoopBackOff: the
	// component is Degraded, not Healthy — replica counts alone would miss it.
	chk := newChecker(t, "",
		deployment("spicebox-operator", 1, 1, 1),
		pod("spicebox-operator-abc", "spicebox-operator", true, 9, "CrashLoopBackOff"),
	)
	rep, err := chk.Snapshot(context.Background())
	require.NoError(t, err)
	op := componentStatus(t, rep, "operator")
	assert.Equal(t, StatusDegraded, op.Status)
	assert.Contains(t, op.Detail, "CrashLoopBackOff")
}

func TestSnapshot_Rollup(t *testing.T) {
	chk := newChecker(t, "",
		deployment("spicebox-operator", 1, 1, 1),
		pod("p1", "spicebox-operator", true, 0, ""),
		pod("p2", "spicebox-webd", true, 0, ""),
		pod("p3", "spicebox-channelsd", false, 0, ""),
	)
	rep, err := chk.Snapshot(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, rep.Rollup.Pods, "all namespace pods counted")
	assert.Equal(t, 2, rep.Rollup.Ready, "only Ready=True pods counted")
	// Metrics API served but no PodMetrics for these pods: usage sums to zero,
	// not "n/a" (which is reserved for the metrics API being unavailable).
	assert.Equal(t, "0m", rep.Rollup.CPU)
	assert.Equal(t, "0 B", rep.Rollup.Memory)
}

// podMetrics builds a PodMetrics with one or more containers, each carrying a
// CPU (millicore-parseable, e.g. "250m") and memory (e.g. "512Mi") usage.
func podMetrics(name string, containers ...map[string]string) *metricsv1beta1.PodMetrics {
	pm := &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace},
	}
	for i, c := range containers {
		usage := corev1.ResourceList{}
		if cpu, ok := c["cpu"]; ok {
			usage[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if mem, ok := c["memory"]; ok {
			usage[corev1.ResourceMemory] = resource.MustParse(mem)
		}
		pm.Containers = append(pm.Containers, metricsv1beta1.ContainerMetrics{
			Name:  fmt.Sprintf("c%d", i),
			Usage: usage,
		})
	}
	return pm
}

func TestSnapshot_RollupCPUMemory_FromMetrics(t *testing.T) {
	// Two pods, multiple containers each: usage sums across every container of
	// every pod in the namespace. 250m+500m+450m = 1200m; 512Mi+512Mi+512Mi =
	// 1536Mi = 1.5 GiB (humanize IEC).
	chk := newChecker(t, "",
		deployment("spicebox-operator", 1, 1, 1),
		podMetrics("spicebox-operator-abc",
			map[string]string{"cpu": "250m", "memory": "512Mi"},
			map[string]string{"cpu": "500m", "memory": "512Mi"},
		),
		podMetrics("spicebox-webd-def",
			map[string]string{"cpu": "450m", "memory": "512Mi"},
		),
	)
	rep, err := chk.Snapshot(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "1200m", rep.Rollup.CPU, "CPU summed across all containers as millicores")
	assert.Equal(t, "1.5 GiB", rep.Rollup.Memory, "memory summed across all containers, human-readable")
}

// errListReader wraps a client.Reader and forces List of PodMetricsList to
// return err, leaving every other List (deployments/statefulsets/pods)
// untouched — simulating an absent/unserved metrics API.
func metricsErrChecker(t *testing.T, err error, objs ...client.Object) *Checker {
	t.Helper()
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*metricsv1beta1.PodMetricsList); ok {
				return err
			}
			return cl.List(ctx, list, opts...)
		},
	})
	return New(c, "", logr.Discard())
}

func TestSnapshot_MetricsUnavailable_DegradesToNA(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "generic List error: CPU/Memory=n/a, snapshot still succeeds",
			err:  errors.New("the server could not find the requested resource (get pods.metrics.k8s.io)"),
		},
		{
			name: "NotFound (no metrics APIService): CPU/Memory=n/a, snapshot still succeeds",
			err:  apierrors.NewNotFound(schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}, ""),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := metricsErrChecker(t, tc.err, deployment("spicebox-operator", 1, 1, 1))
			rep, err := chk.Snapshot(context.Background())
			require.NoError(t, err, "an unavailable metrics API degrades CPU/Memory, it does not fail the snapshot")
			assert.Equal(t, "n/a", rep.Rollup.CPU)
			assert.Equal(t, "n/a", rep.Rollup.Memory)
			// The rest of the snapshot is unaffected.
			assert.Equal(t, StatusHealthy, componentStatus(t, rep, "operator").Status)
		})
	}
}

func TestSnapshot_GraphitiPing(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    Status
	}{
		{
			name:    "2xx -> Healthy",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			want:    StatusHealthy,
		},
		{
			name:    "5xx -> Degraded",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
			want:    StatusDegraded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			chk := newChecker(t, srv.URL)
			rep, err := chk.Snapshot(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.want, componentStatus(t, rep, "graphiti").Status)
		})
	}
}

func TestSnapshot_GraphitiTimeout_Degraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	chk := newChecker(t, srv.URL)
	chk.HTTPClient = &http.Client{Timeout: 30 * time.Millisecond}
	rep, err := chk.Snapshot(context.Background())
	require.NoError(t, err, "a graphiti timeout degrades graphiti, it does not fail the whole snapshot")
	assert.Equal(t, StatusDegraded, componentStatus(t, rep, "graphiti").Status)
}

func TestSnapshot_GraphitiNotConfigured(t *testing.T) {
	chk := newChecker(t, "")
	rep, err := chk.Snapshot(context.Background())
	require.NoError(t, err)
	g := componentStatus(t, rep, "graphiti")
	assert.Equal(t, StatusNotConfigured, g.Status)
	assert.Equal(t, "not configured", g.Detail)
}
