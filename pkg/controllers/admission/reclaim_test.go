package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func warmOn(name, line string, idleFor time.Duration, now time.Time, cpuMilli int64) WarmIdle {
	return WarmIdle{
		Key:       types.NamespacedName{Namespace: "ns", Name: name},
		Line:      line,
		IdleSince: now.Add(-idleFor),
		Pods:      []PlacedPod{{Node: "a", PodDemand: pod(name, cpuMilli)}},
	}
}

// fullNode is one 1-CPU node already holding two 500m pods, so it has no
// headroom until a warm session's pod is freed.
func fullNode() CapacityModel {
	return NewNodeModel(
		[]corev1.Node{testNode("a", "1", "1Gi", "10")},
		[]corev1.Pod{testPod("a", "500m"), testPod("a", "500m")},
	)
}

func TestPlanReclaim(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	head := func(cpu int64) Candidate {
		return Candidate{Key: types.NamespacedName{Namespace: "ns", Name: "head"}, Line: "agent-a", Demand: Demand{Pods: []PodDemand{pod("h", cpu)}}}
	}
	key := func(n string) types.NamespacedName { return types.NamespacedName{Namespace: "ns", Name: n} }

	cases := []struct {
		name    string
		head    Candidate
		warm    []WarmIdle
		model   func() CapacityModel
		minWarm time.Duration
		want    []types.NamespacedName
	}{
		{
			name:  "head fits after freeing the oldest of two eligible: returns only the oldest",
			head:  head(500),
			model: fullNode,
			warm: []WarmIdle{
				warmOn("newer", "agent-a", 10*time.Minute, now, 500),
				warmOn("oldest", "agent-a", 30*time.Minute, now, 500),
			},
			minWarm: 5 * time.Minute,
			want:    []types.NamespacedName{key("oldest")},
		},
		{
			name:  "head needs both eligible sessions: returns both, oldest first",
			head:  head(1000),
			model: fullNode,
			warm: []WarmIdle{
				warmOn("newer", "agent-a", 10*time.Minute, now, 500),
				warmOn("oldest", "agent-a", 30*time.Minute, now, 500),
			},
			minWarm: 5 * time.Minute,
			want:    []types.NamespacedName{key("oldest"), key("newer")},
		},
		{
			name:    "idle-since tie: breaks on key order",
			head:    head(500),
			model:   fullNode,
			warm:    []WarmIdle{warmOn("b", "agent-a", 10*time.Minute, now, 500), warmOn("a", "agent-a", 10*time.Minute, now, 500)},
			minWarm: time.Minute,
			want:    []types.NamespacedName{key("a")},
		},
		{
			name:    "candidate idle 4m with minWarm 5m: not eligible, returns nil",
			head:    head(500),
			model:   fullNode,
			warm:    []WarmIdle{warmOn("young", "agent-a", 4*time.Minute, now, 500)},
			minWarm: 5 * time.Minute,
			want:    nil,
		},
		{
			name:    "other line's warm session: never chosen",
			head:    head(500),
			model:   fullNode,
			warm:    []WarmIdle{warmOn("other", "agent-b", time.Hour, now, 500)},
			minWarm: 5 * time.Minute,
			want:    nil,
		},
		{
			name:    "freeing every eligible session still does not fit: nil, nothing reclaimed",
			head:    head(1500),
			model:   fullNode,
			warm:    []WarmIdle{warmOn("w1", "agent-a", time.Hour, now, 500), warmOn("w2", "agent-a", time.Hour, now, 500)},
			minWarm: 5 * time.Minute,
			want:    nil,
		},
		{
			name:    "head already fits without freeing: nil",
			head:    head(500),
			model:   func() CapacityModel { return NewNodeModel([]corev1.Node{testNode("a", "1", "1Gi", "10")}, nil) },
			warm:    []WarmIdle{warmOn("w1", "agent-a", time.Hour, now, 500)},
			minWarm: 5 * time.Minute,
			want:    nil,
		},
		{
			name:    "unknown capacity: nil, unknown capacity already admits",
			head:    head(500),
			model:   func() CapacityModel { return NewNodeModel(nil, nil) },
			warm:    []WarmIdle{warmOn("w1", "agent-a", time.Hour, now, 500)},
			minWarm: 5 * time.Minute,
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PlanReclaim(tc.head, tc.warm, tc.model(), now, tc.minWarm))
		})
	}

	t.Run("plan does not mutate the caller's model", func(t *testing.T) {
		m := fullNode()
		w := []WarmIdle{warmOn("w1", "agent-a", time.Hour, now, 500)}
		assert.Equal(t, []types.NamespacedName{key("w1")}, PlanReclaim(head(500), w, m, now, time.Minute))
		assert.False(t, m.Reserve(head(500).Demand), "caller's model must still be full after planning")
	})
}
