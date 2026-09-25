package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testScheme returns a scheme with the core types the fake client needs.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	return s
}

// allocNode builds a Node reporting the given allocatable capacity.
func allocNode(t *testing.T, name, cpu, mem string) *corev1.Node {
	t.Helper()
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}},
	}
}

// bundlePod builds a bundle pod whose single container requests cpu/mem.
func bundlePod(t *testing.T, ns, name, container, cpu, mem string) *corev1.Pod {
	t.Helper()
	req := corev1.ResourceList{}
	if cpu != "" {
		req[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		req[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: container, Resources: corev1.ResourceRequirements{Requests: req}},
		}},
	}
}

func TestProvablyUnschedulableBundle(t *testing.T) {
	const ns = "test-ns"

	cases := []struct {
		name     string
		nodes    []*corev1.Node
		pods     []*corev1.Pod
		podNames []string
		want     bool
		contains string
	}{
		{
			name:     "memory request above the largest node: flagged with a reason",
			nodes:    []*corev1.Node{allocNode(t, "oap-desktop", "4", "3800Mi")},
			pods:     []*corev1.Pod{bundlePod(t, ns, "bundle-codelike", "codelike", "1", "4Gi")},
			podNames: []string{"bundle-codelike"},
			want:     true,
			contains: "memory",
		},
		{
			name:     "fits: not flagged",
			nodes:    []*corev1.Node{allocNode(t, "oap-desktop", "4", "3800Mi")},
			pods:     []*corev1.Pod{bundlePod(t, ns, "bundle-gitlike", "gitlike", "500m", "256Mi")},
			podNames: []string{"bundle-gitlike"},
			want:     false,
		},
		{
			name:     "no nodes readable: fail-safe, never flagged",
			nodes:    nil,
			pods:     []*corev1.Pod{bundlePod(t, ns, "bundle-codelike", "codelike", "1", "4Gi")},
			podNames: []string{"bundle-codelike"},
			want:     false,
		},
		{
			name:     "no pod names: nothing to check",
			nodes:    []*corev1.Node{allocNode(t, "oap-desktop", "4", "3800Mi")},
			podNames: nil,
			want:     false,
		},
		{
			name:     "pod not created yet: fail-safe, never flagged",
			nodes:    []*corev1.Node{allocNode(t, "oap-desktop", "4", "3800Mi")},
			podNames: []string{"bundle-missing"},
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{}
			for _, n := range tc.nodes {
				objs = append(objs, n)
			}
			for _, p := range tc.pods {
				objs = append(objs, p)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(objs...).Build()
			r := &Reconciler{Client: c}

			reason, got := r.provablyUnschedulableBundle(context.Background(), ns, tc.podNames)
			assert.Equal(t, tc.want, got)
			if tc.contains != "" {
				assert.Contains(t, reason, tc.contains)
			}
			if !tc.want {
				assert.Empty(t, reason)
			}
		})
	}
}

// multiContainerPod builds a bundle pod with several app containers, plus
// optional init containers, each requesting the given memory.
func multiContainerPod(t *testing.T, ns, name string, appMem []string, initMem []string) *corev1.Pod {
	t.Helper()
	mk := func(prefix string, mems []string) []corev1.Container {
		out := make([]corev1.Container, 0, len(mems))
		for i, m := range mems {
			out = append(out, corev1.Container{
				Name: prefix + string(rune('a'+i)),
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse(m),
				}},
			})
		}
		return out
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{
			Containers:     mk("app-", appMem),
			InitContainers: mk("init-", initMem),
		},
	}
}

// Kubernetes schedules a POD, and a pod's effective request is the SUM of its
// app containers — not each container weighed alone. Checking per-container
// under-detects: three 2Gi containers each "fit" a 4Gi node while the pod they
// belong to needs 6Gi and can never be placed, so the session waits out the
// full bundleReadyDeadline instead of failing fast with a usable reason.
func TestProvablyUnschedulableBundle_SumsAppContainers(t *testing.T) {
	const ns = "test-ns"
	node := allocNode(t, "n1", "8", "4Gi")
	pod := multiContainerPod(t, ns, "bundle-pod", []string{"2Gi", "2Gi", "2Gi"}, nil)

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node, pod).Build()
	r := &Reconciler{Client: c}

	reason, got := r.provablyUnschedulableBundle(context.Background(), ns, []string{"bundle-pod"})
	assert.True(t, got, "6Gi of app containers cannot be placed on a 4Gi node")
	assert.Contains(t, reason, "6Gi", "the reason must cite the pod's total, not one container's share")
}

// An init container runs BEFORE the app containers and does not hold its
// request alongside them, so the pod's effective request is max(sum(app),
// largest init) — not their sum. Adding them would falsely condemn a pod that
// schedules perfectly well.
func TestProvablyUnschedulableBundle_InitContainerIsAMaxNotAnAddend(t *testing.T) {
	const ns = "test-ns"
	node := allocNode(t, "n1", "8", "4Gi")
	// sum(app)=3Gi, largest init=3Gi -> effective 3Gi, fits a 4Gi node.
	pod := multiContainerPod(t, ns, "bundle-pod", []string{"1Gi", "2Gi"}, []string{"3Gi"})

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node, pod).Build()
	r := &Reconciler{Client: c}

	_, got := r.provablyUnschedulableBundle(context.Background(), ns, []string{"bundle-pod"})
	assert.False(t, got, "3Gi effective request fits; summing init with app would wrongly fail it")
}

// A single init container larger than the node can never run, even though every
// app container fits.
func TestProvablyUnschedulableBundle_OversizedInitContainerIsCaught(t *testing.T) {
	const ns = "test-ns"
	node := allocNode(t, "n1", "8", "4Gi")
	pod := multiContainerPod(t, ns, "bundle-pod", []string{"512Mi"}, []string{"6Gi"})

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node, pod).Build()
	r := &Reconciler{Client: c}

	_, got := r.provablyUnschedulableBundle(context.Background(), ns, []string{"bundle-pod"})
	assert.True(t, got, "a 6Gi init container cannot run on a 4Gi node")
}
