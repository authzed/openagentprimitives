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
