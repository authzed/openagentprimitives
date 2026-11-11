package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// slotModel is a fake CapacityModel that admits a fixed number of pods.
type slotModel struct {
	known bool
	slots int
}

func (m *slotModel) Known() bool { return m.known }
func (m *slotModel) Reserve(d Demand) bool {
	if !m.known {
		return true
	}
	if len(d.Pods) > m.slots {
		return false
	}
	m.slots -= len(d.Pods)
	return true
}
func (m *slotModel) Free([]PlacedPod)       {}
func (m *slotModel) Clone() CapacityModel   { c := *m; return &c }
func (m *slotModel) Totals() (int64, int64) { return 0, 0 }

func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "ns", Name: name}
}

func cand(name, line string, joinedSec int, child bool, cpuMilli ...int64) Candidate {
	d := Demand{}
	for i, c := range cpuMilli {
		d.Pods = append(d.Pods, pod(name+string(rune('0'+i)), c))
	}
	return Candidate{Key: key(name), Line: line, JoinedAt: t0.Add(time.Duration(joinedSec) * time.Second), Child: child, Demand: d}
}

func holder(name, line string, child bool, cpuMilli int64) Holder {
	return Holder{Key: key(name), Line: line, Child: child, Demand: Demand{Pods: []PodDemand{pod(name, cpuMilli)}}}
}

func keys(names ...string) []types.NamespacedName {
	var out []types.NamespacedName
	for _, n := range names {
		out = append(out, key(n))
	}
	return out
}

func TestDecide(t *testing.T) {
	oneNodeModel := func(cpu string) CapacityModel {
		return NewNodeModel([]corev1.Node{testNode("n1", cpu, "8Gi", "10")}, nil)
	}
	cases := []struct {
		name  string
		in    func() Input
		admit []types.NamespacedName
	}{
		{
			name: "burst on one class, room for two: first two by join time admitted, rest keep order",
			in: func() Input {
				return Input{
					Waiting: []Candidate{cand("c", "l", 3, false, 100), cand("a", "l", 1, false, 100), cand("b", "l", 2, false, 100)},
					Model:   &slotModel{known: true, slots: 2},
				}
			},
			admit: keys("a", "b"),
		},
		{
			name: "smaller later session does not jump a larger head in the same line",
			in: func() Input {
				return Input{
					Waiting: []Candidate{cand("big", "l", 1, false, 100, 100, 100), cand("small", "l", 2, false, 100)},
					Model:   &slotModel{known: true, slots: 2},
				}
			},
			admit: nil,
		},
		{
			name: "head of line A does not fit, head of line B does: B admitted",
			in: func() Input {
				return Input{
					Waiting: []Candidate{cand("a", "A", 1, false, 4000), cand("b", "B", 2, false, 500)},
					Model:   oneNodeModel("2"),
				}
			},
			admit: keys("b"),
		},
		{
			name: "two lines both fit one more: lower dominant share admitted first",
			in: func() Input {
				return Input{
					Waiting:       []Candidate{cand("a", "A", 1, false, 100), cand("b", "B", 2, false, 100)},
					Holders:       []Holder{holder("ha", "A", false, 4000), holder("hb", "B", false, 1000)},
					Model:         &slotModel{known: true, slots: 1},
					TotalCPUMilli: 10000, TotalMemBytes: 1 << 40,
				}
			},
			admit: keys("b"),
		},
		{
			name: "equal shares across lines: admitted in head JoinedAt order, then by line name",
			in: func() Input {
				return Input{
					Waiting: []Candidate{cand("c", "C", 2, false, 100), cand("a", "A", 2, false, 100), cand("b", "B", 1, false, 100)},
					Model:   &slotModel{known: true, slots: 3},
				}
			},
			admit: keys("b", "a", "c"),
		},
		{
			name: "one free slot: a later child is admitted ahead of an earlier non-child in its line",
			in: func() Input {
				return Input{
					Waiting: []Candidate{cand("early", "l", 1, false, 100), cand("kid", "l", 2, true, 100)},
					Model:   &slotModel{known: true, slots: 1},
				}
			},
			admit: keys("kid"),
		},
		{
			name: "cap 2 with 2 holders: non-child head waits, child head admitted",
			in: func() Input {
				return Input{
					Waiting: []Candidate{cand("w", "l", 1, false, 100), cand("kid", "l", 2, true, 100)},
					Holders: []Holder{holder("h1", "l", false, 100), holder("h2", "l", false, 100)},
					Caps:    map[string]int32{"l": 2},
					Model:   &slotModel{known: true, slots: 5},
				}
			},
			admit: keys("kid"),
		},
		{
			name: "cap counts non-child holders admitted in the same pass: cap 1 admits one of two",
			in: func() Input {
				return Input{
					Waiting: []Candidate{cand("a", "l", 1, false, 100), cand("b", "l", 2, false, 100)},
					Caps:    map[string]int32{"l": 1},
					Model:   &slotModel{known: true, slots: 5},
				}
			},
			admit: keys("a"),
		},
		{
			name: "unplaced holder demand is reserved before waiting heads: nothing new admitted",
			in: func() Input {
				return Input{
					Waiting: []Candidate{cand("w", "l", 1, false, 600)},
					Holders: []Holder{{
						Key: key("h"), Line: "l",
						Demand:   Demand{Pods: []PodDemand{pod("h", 600)}},
						Unplaced: Demand{Pods: []PodDemand{pod("h", 600)}},
					}},
					Model: oneNodeModel("1"),
				}
			},
			admit: nil,
		},
		{
			name: "unknown capacity: all admitted in order, cap still applies",
			in: func() Input {
				return Input{
					Waiting: []Candidate{
						cand("a", "l", 1, false, 100), cand("b", "l", 2, false, 100), cand("c", "l", 3, false, 100),
						cand("x", "free", 4, false, 100),
					},
					Caps:  map[string]int32{"l": 2},
					Model: &slotModel{known: false},
				}
			},
			admit: keys("a", "b", "x"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.in())
			assert.Equal(t, tc.admit, got.Admit)
		})
	}
}

func TestDecideDoesNotMutateCallerSlices(t *testing.T) {
	waiting := []Candidate{cand("b", "l", 2, false, 100), cand("a", "l", 1, false, 100)}
	holders := []Holder{holder("h", "l", false, 100)}
	Decide(Input{Waiting: waiting, Holders: holders, Model: &slotModel{known: true, slots: 0}})
	assert.Equal(t, key("b"), waiting[0].Key, "Waiting must keep the caller's order")
	assert.Len(t, holders, 1)
}

func TestDecidePositions(t *testing.T) {
	got := Decide(Input{
		Waiting: []Candidate{
			cand("a1", "A", 1, false, 100), cand("a2", "A", 2, false, 100), cand("a3", "A", 3, false, 100),
			cand("b1", "B", 1, false, 100), cand("b2", "B", 2, false, 100),
		},
		Model: &slotModel{known: true, slots: 2},
	})
	require.ElementsMatch(t, keys("a1", "b1"), got.Admit)
	assert.Equal(t, map[types.NamespacedName]Position{
		key("a2"): {Index: 1, LineLength: 2},
		key("a3"): {Index: 2, LineLength: 2},
		key("b2"): {Index: 1, LineLength: 1},
	}, got.Positions, "positions are 1-based per line, cover only still-waiting candidates")
}

// CapBlocked names the lines whose head is still waiting because the line is
// at its cap, not because it does not fit — the lines a cap reclaim can help.
func TestDecideCapBlocked(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want map[string]bool
	}{
		{
			name: "cap 1 with one holder, room to spare: the line is cap-blocked",
			in: Input{
				Waiting: []Candidate{cand("w", "l", 1, false, 100)},
				Holders: []Holder{holder("h", "l", false, 100)},
				Caps:    map[string]int32{"l": 1},
				Model:   &slotModel{known: true, slots: 5},
			},
			want: map[string]bool{"l": true},
		},
		{
			name: "uncapped line whose head does not fit: not cap-blocked",
			in: Input{
				Waiting: []Candidate{cand("w", "l", 1, false, 100, 100)},
				Model:   &slotModel{known: true, slots: 1},
			},
			want: map[string]bool{},
		},
		{
			name: "under its cap, head does not fit: not cap-blocked",
			in: Input{
				Waiting: []Candidate{cand("w", "l", 1, false, 100, 100)},
				Holders: []Holder{holder("h", "l", false, 100)},
				Caps:    map[string]int32{"l": 2},
				Model:   &slotModel{known: true, slots: 1},
			},
			want: map[string]bool{},
		},
		{
			name: "cap reached by this pass's own admission: cap-blocked",
			in: Input{
				Waiting: []Candidate{cand("a", "l", 1, false, 100), cand("b", "l", 2, false, 100)},
				Caps:    map[string]int32{"l": 1},
				Model:   &slotModel{known: true, slots: 5},
			},
			want: map[string]bool{"l": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Decide(tc.in).CapBlocked)
		})
	}
}

// A holder's unplaced pods are reserved one at a time, best effort: one that
// cannot be placed (the model is wrong about it, or it is waiting on a node
// to free) must not stop the others from being reserved, or a waiting head
// would be admitted into space the holder's other pods are about to take.
func TestDecideReservesHolderUnplacedPodsIndividually(t *testing.T) {
	in := Input{
		Waiting: []Candidate{cand("w", "l", 1, false, 600)},
		Holders: []Holder{{
			Key: key("h"), Line: "l",
			Demand: Demand{Pods: []PodDemand{pod("h-small", 600), pod("h-huge", 4000)}},
			// h-huge fits no node; h-small will land on n1.
			Unplaced: Demand{Pods: []PodDemand{pod("h-small", 600), pod("h-huge", 4000)}},
		}},
		Model: NewNodeModel([]corev1.Node{testNode("n1", "1", "8Gi", "10")}, nil),
	}
	assert.Empty(t, Decide(in).Admit, "the holder's placeable pod is reserved even though its other pod is not")
}
