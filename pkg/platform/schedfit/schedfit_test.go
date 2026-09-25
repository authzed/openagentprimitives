package schedfit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const gib = int64(1024 * 1024 * 1024)

// node builds a Node with the given allocatable. Empty strings omit the
// dimension entirely, which is how a real node that does not report one looks.
func node(name, cpu, mem, eph string) corev1.Node {
	t := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	t.Status.Allocatable = corev1.ResourceList{}
	if cpu != "" {
		t.Status.Allocatable[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		t.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	if eph != "" {
		t.Status.Allocatable[corev1.ResourceEphemeralStorage] = resource.MustParse(eph)
	}
	return t
}

// reqList builds a request list, omitting empty dimensions.
func reqList(cpu, mem, eph string) corev1.ResourceList {
	rl := corev1.ResourceList{}
	if cpu != "" {
		rl[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		rl[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	if eph != "" {
		rl[corev1.ResourceEphemeralStorage] = resource.MustParse(eph)
	}
	return rl
}

// pod builds a scheduled pod with one container requesting the given amounts.
func pod(name, nodeName, cpu, mem string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{Requests: reqList(cpu, mem, "")}}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestCeilingFromNodes(t *testing.T) {
	t.Run("per-dimension maxima are taken independently across nodes", func(t *testing.T) {
		c := CeilingFromNodes([]corev1.Node{
			node("small-cpu-big-mem", "940m", "3Gi", "20Gi"),
			node("big-cpu-small-mem", "1500m", "2Gi", "10Gi"),
		})
		assert.True(t, c.Known)
		assert.Equal(t, int64(1500), c.CPUMilli, "largest cpu in millicores, regardless of which node")
		assert.Equal(t, 3*gib, c.MemBytes, "largest memory in bytes")
		assert.Equal(t, 20*gib, c.EphemeralBytes, "largest ephemeral-storage in bytes")
		assert.Equal(t, "small-cpu-big-mem", c.Node, "Node names the memory-ceiling node")
		assert.Contains(t, c.Source, "small-cpu-big-mem")
	})

	t.Run("no nodes: Known=false so callers skip the check", func(t *testing.T) {
		c := CeilingFromNodes(nil)
		assert.False(t, c.Known)
		assert.Zero(t, c.CPUMilli)
		assert.Zero(t, c.MemBytes)
		assert.NotEmpty(t, c.Source, "Source must explain why it is unknown")
	})

	t.Run("nodes reporting no allocatable at all: Known=false", func(t *testing.T) {
		c := CeilingFromNodes([]corev1.Node{node("blank", "", "", "")})
		assert.False(t, c.Known)
	})
}

func TestHeadroomOn(t *testing.T) {
	n := node("oap-desktop", "4", "4Gi", "20Gi")

	t.Run("subtracts requests of pods on this node", func(t *testing.T) {
		h := HeadroomOn(n, []corev1.Pod{
			pod("postgres", "oap-desktop", "100m", "256Mi", corev1.PodRunning),
			pod("neo4j", "oap-desktop", "100m", "512Mi", corev1.PodRunning),
		})
		assert.True(t, h.Known)
		assert.Equal(t, int64(4000-200), h.CPUMilli)
		assert.Equal(t, 4*gib-768*1024*1024, h.MemBytes)
	})

	t.Run("ignores pods on other nodes and terminal pods", func(t *testing.T) {
		h := HeadroomOn(n, []corev1.Pod{
			pod("elsewhere", "other-node", "1", "1Gi", corev1.PodRunning),
			pod("done", "oap-desktop", "1", "1Gi", corev1.PodSucceeded),
			pod("dead", "oap-desktop", "1", "1Gi", corev1.PodFailed),
		})
		assert.Equal(t, int64(4000), h.CPUMilli)
		assert.Equal(t, 4*gib, h.MemBytes)
	})

	t.Run("over-committed node floors at zero, never negative", func(t *testing.T) {
		h := HeadroomOn(n, []corev1.Pod{pod("hog", "oap-desktop", "8", "8Gi", corev1.PodRunning)})
		assert.Zero(t, h.CPUMilli)
		assert.Zero(t, h.MemBytes)
	})

	t.Run("init container larger than the container sum wins (k8s effective request)", func(t *testing.T) {
		p := pod("initheavy", "oap-desktop", "100m", "128Mi", corev1.PodRunning)
		p.Spec.InitContainers = []corev1.Container{{
			Name:      "setup",
			Resources: corev1.ResourceRequirements{Requests: reqList("", "2Gi", "")},
		}}
		h := HeadroomOn(n, []corev1.Pod{p})
		assert.Equal(t, 4*gib-2*gib, h.MemBytes, "the 2Gi init request replaces the 128Mi container sum")
		assert.Equal(t, int64(4000-100), h.CPUMilli, "cpu is unaffected: the init container requests none")
	})
}

func TestExceeds(t *testing.T) {
	c := Ceiling{Known: true, CPUMilli: 940, MemBytes: 3 * gib, EphemeralBytes: 20 * gib, Node: "n1", Source: "largest node n1"}

	cases := []struct {
		name     string
		label    string
		req      corev1.ResourceList
		ceiling  Ceiling
		exceeded bool
		contains []string
	}{
		{
			name:  "cpu above the largest node: flagged, reason names the dimension",
			label: `container "codelike"`, req: reqList("1", "", ""), ceiling: c,
			exceeded: true, contains: []string{"codelike", "cpu"},
		},
		{
			name:  "memory above the largest node: flagged",
			label: "SpiceboxClass codelike-bundle", req: reqList("100m", "8Gi", ""), ceiling: c,
			exceeded: true, contains: []string{"codelike-bundle", "memory"},
		},
		{
			name:  "ephemeral-storage above the largest node: flagged",
			label: "SpiceboxClass big-disk", req: reqList("", "", "50Gi"), ceiling: c,
			exceeded: true, contains: []string{"ephemeral-storage"},
		},
		{
			name:  "everything fits: not flagged",
			label: `container "gitlike"`, req: reqList("500m", "256Mi", "500Mi"), ceiling: c,
			exceeded: false,
		},
		{
			name:  "unknown ceiling: never flagged, missing data must not block",
			label: "x", req: reqList("99", "99Gi", ""), ceiling: Ceiling{},
			exceeded: false,
		},
		{
			name:  "zero dimension is skipped, not treated as offering nothing",
			label: "x", req: reqList("99", "", ""), ceiling: Ceiling{Known: true, MemBytes: 3 * gib},
			exceeded: false,
		},
		{
			name:  "request omits the dimension entirely: not flagged",
			label: "x", req: reqList("", "", ""), ceiling: c,
			exceeded: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ex := Exceeds(tc.label, tc.req, tc.ceiling)
			assert.Equal(t, tc.exceeded, ex)
			for _, want := range tc.contains {
				assert.Contains(t, reason, want)
			}
			if !tc.exceeded {
				assert.Empty(t, reason, "a non-exceeding check returns no reason")
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	assert.Equal(t, "4.0 GiB", HumanBytes(4*gib))
	assert.Equal(t, "0 B", HumanBytes(-1), "negative input must not overflow the unsigned conversion")
}

// A native sidecar (init container with restartPolicy Always) runs for the
// pod's whole lifetime alongside the app containers, so it ADDS to the pod's
// cost rather than merely setting a floor the way an ordinary init container
// does. Treating one as a floor under-counts the pod and hands back headroom
// the node does not actually have.
func TestEffectiveRequests_NativeSidecarAddsRatherThanSettingAFloor(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	pod := corev1.Pod{Spec: corev1.PodSpec{
		NodeName: "n1",
		Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		}}},
		InitContainers: []corev1.Container{{
			Name:          "sidecar",
			RestartPolicy: &always,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
		}},
	}}
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		}},
	}

	h := HeadroomOn(node, []corev1.Pod{pod})
	assert.Equal(t, int64(2)*1024*1024*1024, h.MemBytes,
		"app(1Gi)+sidecar(1Gi)=2Gi booked of 4Gi; treating the sidecar as a floor would leave 3Gi")
}
