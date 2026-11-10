package admission

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testNode(name, cpu, mem, pods string) corev1.Node {
	rl := corev1.ResourceList{}
	if cpu != "" {
		rl[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		rl[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	if pods != "" {
		rl[corev1.ResourcePods] = resource.MustParse(pods)
	}
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Allocatable: rl}}
}

func testPod(node, cpu string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p-" + node + "-" + cpu},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
		}}},
	}
}

func pod(name string, cpuMilli int64) PodDemand {
	return PodDemand{Name: name, CPUMilli: cpuMilli, MemBytes: 1 << 20}
}

func TestNodeModelReserve(t *testing.T) {
	sixNodes := func() ([]corev1.Node, []corev1.Pod) {
		var nodes []corev1.Node
		var pods []corev1.Pod
		for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
			nodes = append(nodes, testNode(n, "1", "1Gi", "10"))
			pods = append(pods, testPod(n, "600m"))
		}
		return nodes, pods
	}
	cases := []struct {
		name   string
		build  func() ([]corev1.Node, []corev1.Pod)
		demand Demand
		then   *Demand // second Reserve, if any
		want   bool
		wantTo *bool
		known  bool
	}{
		{
			name:   "aggregate free but no single node fits: false",
			build:  sixNodes,
			demand: Demand{Pods: []PodDemand{pod("big", 1000)}},
			want:   false, known: true,
		},
		{
			name: "two pods split across two nodes: true, both committed; identical second Reserve fails",
			build: func() ([]corev1.Node, []corev1.Pod) {
				return []corev1.Node{testNode("a", "1", "1Gi", "10"), testNode("b", "1", "1Gi", "10")}, nil
			},
			demand: Demand{Pods: []PodDemand{pod("x", 800), pod("y", 800)}},
			then:   &Demand{Pods: []PodDemand{pod("x", 800), pod("y", 800)}},
			want:   true, wantTo: ptr(false), known: true,
		},
		{
			name: "pod-count exhausted on the only roomy node: false",
			build: func() ([]corev1.Node, []corev1.Pod) {
				return []corev1.Node{testNode("a", "8", "8Gi", "1")}, []corev1.Pod{testPod("a", "100m")}
			},
			demand: Demand{Pods: []PodDemand{pod("x", 100)}},
			want:   false, known: true,
		},
		{
			name: "missing pods allocatable is unlimited slots: true",
			build: func() ([]corev1.Node, []corev1.Pod) {
				return []corev1.Node{testNode("a", "8", "8Gi", "")}, nil
			},
			demand: Demand{Pods: []PodDemand{pod("x", 100), pod("y", 100)}},
			want:   true, known: true,
		},
		{
			name: "unschedulable node is skipped: false",
			build: func() ([]corev1.Node, []corev1.Pod) {
				n := testNode("a", "8", "8Gi", "10")
				n.Spec.Unschedulable = true
				return []corev1.Node{n, testNode("b", "100m", "8Gi", "10")}, nil
			},
			demand: Demand{Pods: []PodDemand{pod("x", 1000)}},
			want:   false, known: true,
		},
		{
			name: "unknown (no allocatable reported): Known()==false and Reserve true",
			build: func() ([]corev1.Node, []corev1.Pod) {
				return []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}}, nil
			},
			demand: Demand{Pods: []PodDemand{pod("x", 1<<40)}},
			want:   true, known: false,
		},
		{
			name: "failed Reserve commits nothing: a later fitting Reserve succeeds",
			build: func() ([]corev1.Node, []corev1.Pod) {
				return []corev1.Node{testNode("a", "1", "1Gi", "10")}, nil
			},
			// first pod would fit, second cannot; nothing may stay reserved.
			demand: Demand{Pods: []PodDemand{pod("x", 900), pod("y", 900)}},
			then:   &Demand{Pods: []PodDemand{pod("z", 900)}},
			want:   false, wantTo: ptr(true), known: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, pods := tc.build()
			m := NewNodeModel(nodes, pods)
			assert.Equal(t, tc.known, m.Known())
			assert.Equal(t, tc.want, m.Reserve(tc.demand))
			if tc.then != nil {
				assert.Equal(t, *tc.wantTo, m.Reserve(*tc.then))
			}
		})
	}
}

func TestNodeModelReserveEphemeralStorage(t *testing.T) {
	n := testNode("a", "8", "8Gi", "10")
	n.Status.Allocatable[corev1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
	m := NewNodeModel([]corev1.Node{n}, nil)
	assert.False(t, m.Reserve(Demand{Pods: []PodDemand{{Name: "x", EphemeralBytes: 2 << 30}}}),
		"ephemeral storage beyond the node's allocatable must not fit")
}

func TestNodeModelFree(t *testing.T) {
	m := NewNodeModel([]corev1.Node{testNode("a", "1", "1Gi", "10")}, nil)
	d := Demand{Pods: []PodDemand{pod("x", 800)}}
	require.True(t, m.Reserve(d))
	require.False(t, m.Reserve(d), "node is full")

	m.Free([]PlacedPod{{Node: "a", PodDemand: pod("x", 800)}})
	assert.True(t, m.Reserve(d), "freeing the placed pod makes the Reserve succeed")
}

func TestNodeModelCloneIsIndependent(t *testing.T) {
	m := NewNodeModel([]corev1.Node{testNode("a", "1", "1Gi", "10")}, nil)
	c := m.Clone()
	d := Demand{Pods: []PodDemand{pod("x", 800)}}
	require.True(t, c.Reserve(d))
	assert.True(t, m.Reserve(d), "reserving on the clone must not affect the original")
}

func TestUnknownModelCloneStaysUnknown(t *testing.T) {
	m := NewNodeModel(nil, nil)
	c := m.Clone()
	assert.False(t, c.Known())
	c.Free([]PlacedPod{{Node: "a"}})
	assert.True(t, c.Reserve(Demand{Pods: []PodDemand{pod("x", 1<<40)}}))
}

func ptr[T any](v T) *T { return &v }

func TestKubeCapacitySourceSnapshot(t *testing.T) {
	n := testNode("a", "1", "1Gi", "10")
	p := testPod("a", "600m")
	c := fake.NewClientBuilder().WithObjects(&n, &p).Build()

	m, err := KubeCapacitySource{Reader: c}.Snapshot(t.Context())
	require.NoError(t, err)
	assert.True(t, m.Known())
	assert.False(t, m.Reserve(Demand{Pods: []PodDemand{pod("x", 500)}}), "the listed pod's request is booked")
	assert.True(t, m.Reserve(Demand{Pods: []PodDemand{pod("x", 300)}}))
}

func TestNodeModelTotals(t *testing.T) {
	cordoned := testNode("c", "4", "4Gi", "10")
	cordoned.Spec.Unschedulable = true
	cases := []struct {
		name             string
		nodes            []corev1.Node
		wantCPU, wantMem int64
	}{
		{
			name:    "two schedulable nodes: allocatable summed, unaffected by pods placed on them",
			nodes:   []corev1.Node{testNode("a", "1", "1Gi", "10"), testNode("b", "2", "2Gi", "10")},
			wantCPU: 3000, wantMem: 3 << 30,
		},
		{
			name:    "an unschedulable node: excluded from the totals",
			nodes:   []corev1.Node{testNode("a", "1", "1Gi", "10"), cordoned},
			wantCPU: 1000, wantMem: 1 << 30,
		},
		{name: "no nodes: unknown, zero totals"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewNodeModel(tc.nodes, []corev1.Pod{testPod("a", "600m")})
			require.True(t, m.Reserve(Demand{Pods: []PodDemand{pod("x", 100)}}), "Reserve")
			cpu, mem := m.Totals()
			assert.Equal(t, tc.wantCPU, cpu, "cpu total")
			assert.Equal(t, tc.wantMem, mem, "memory total")
			cpu, mem = m.Clone().Totals()
			assert.Equal(t, tc.wantCPU, cpu, "clone cpu total")
			assert.Equal(t, tc.wantMem, mem, "clone memory total")
		})
	}
}

// A node's NoSchedule/NoExecute taints keep a pod off it unless the pod
// tolerates them, as the scheduler would; PreferNoSchedule does not. Without
// this the model counts a tainted pool as room, admits a set the scheduler
// then cannot place, and the misfit safety net reaps and re-admits it in a
// loop.
func TestNodeModelReserveRespectsTaints(t *testing.T) {
	gpuTaint := corev1.Taint{Key: "nvidia.com/gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	tainted := func(effect corev1.TaintEffect) corev1.Node {
		n := testNode("gpu", "8", "8Gi", "10")
		taint := gpuTaint
		taint.Effect = effect
		n.Spec.Taints = []corev1.Taint{taint}
		return n
	}
	tolerating := pod("p", 1000)
	tolerating.Tolerations = []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	cases := []struct {
		name  string
		nodes []corev1.Node
		pod   PodDemand
		want  bool
	}{
		{"only a NoSchedule-tainted node, pod does not tolerate: does not fit", []corev1.Node{tainted(corev1.TaintEffectNoSchedule)}, pod("p", 1000), false},
		{"only a NoExecute-tainted node, pod does not tolerate: does not fit", []corev1.Node{tainted(corev1.TaintEffectNoExecute)}, pod("p", 1000), false},
		{"only a NoSchedule-tainted node, pod tolerates it: fits", []corev1.Node{tainted(corev1.TaintEffectNoSchedule)}, tolerating, true},
		{"only a PreferNoSchedule-tainted node: fits (a preference, not a bar)", []corev1.Node{tainted(corev1.TaintEffectPreferNoSchedule)}, pod("p", 1000), true},
		{"a tainted big node beside an untainted small one, pod too big for the small: does not fit",
			[]corev1.Node{tainted(corev1.TaintEffectNoSchedule), testNode("small", "500m", "1Gi", "10")}, pod("p", 1000), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewNodeModel(tc.nodes, nil)
			require.True(t, m.Known())
			assert.Equal(t, tc.want, m.Reserve(Demand{Pods: []PodDemand{tc.pod}}))
		})
	}
}

func TestPodDemandFor(t *testing.T) {
	tol := corev1.Toleration{Key: "pool", Operator: corev1.TolerationOpEqual, Value: "agents", Effect: corev1.TaintEffectNoSchedule}
	spec := corev1.PodSpec{
		Tolerations: []corev1.Toleration{tol},
		Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("64Mi"),
		}}}},
	}
	got := PodDemandFor("runner", &spec)
	assert.Equal(t, "runner", got.Name)
	assert.Equal(t, int64(250), got.CPUMilli)
	assert.Equal(t, int64(64<<20), got.MemBytes)
	assert.Equal(t, []corev1.Toleration{tol}, got.Tolerations, "the pod's tolerations ride along for the node model")
}
