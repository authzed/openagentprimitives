package admission

import (
	"cmp"
	"slices"

	"k8s.io/apimachinery/pkg/types"
)

// Holder is an admitted, not-yet-reclaimed session. Unplaced is the part of
// its pod set not yet bound to a node; Decide reserves it first so two passes
// do not count the same free space.
type Holder struct {
	Key      types.NamespacedName
	Line     string
	Child    bool
	Demand   Demand
	Unplaced Demand
}

// Input is everything one admission pass needs.
type Input struct {
	Waiting []Candidate
	Holders []Holder
	// Caps is the per-line limit on non-child holders. A line with no entry,
	// or a cap <= 0, is uncapped.
	Caps  map[string]int32
	Model CapacityModel
	// TotalCPUMilli and TotalMemBytes normalize usage into a dominant share.
	// Zero totals make every share zero.
	TotalCPUMilli, TotalMemBytes int64
}

// Position is a waiting session's place in its line. Index is 1-based.
type Position struct {
	Index, LineLength int32
}

// Decision is the outcome of one pass: who to admit now, and where everyone
// still waiting stands in their line.
type Decision struct {
	Admit     []types.NamespacedName
	Positions map[types.NamespacedName]Position
}

// lineState is one line's remaining queue and what it already holds.
type lineState struct {
	name     string
	queue    []Candidate
	cpuMilli int64
	memBytes int64
	nonChild int32
	capacity int32 // <= 0: uncapped
}

func (l *lineState) eligible() bool {
	if len(l.queue) == 0 {
		return false
	}
	return l.queue[0].Child || l.capacity <= 0 || l.nonChild < l.capacity
}

// Decide admits waiting sessions, whole pod set at a time, repeatedly picking
// the eligible line with the lowest dominant share of the cluster and
// admitting its head if the capacity model can place it. A head that does not
// fit blocks only its own line (strict FIFO within a line). Children are
// ordered ahead of others in their line and are never blocked by, nor counted
// toward, the cap.
//
// Decide mutates in.Model: Unplaced holder demand and every admitted
// candidate's demand are reserved on it. It does not modify in.Waiting or
// in.Holders.
func Decide(in Input) Decision {
	for _, h := range in.Holders {
		// A failure means the pods already landed on nodes the snapshot
		// accounts for; the holder is admitted either way.
		_ = in.Model.Reserve(h.Unplaced)
	}

	lines := map[string]*lineState{}
	line := func(name string) *lineState {
		l, ok := lines[name]
		if !ok {
			l = &lineState{name: name, capacity: in.Caps[name]}
			lines[name] = l
		}
		return l
	}
	for _, h := range in.Holders {
		l := line(h.Line)
		cpu, mem := h.Demand.Total()
		l.cpuMilli += cpu
		l.memBytes += mem
		if !h.Child {
			l.nonChild++
		}
	}
	for _, c := range in.Waiting {
		l := line(c.Line)
		l.queue = append(l.queue, c)
	}
	for _, l := range lines {
		Order(l.queue)
	}

	share := func(l *lineState) float64 {
		var s float64
		if in.TotalCPUMilli > 0 {
			s = float64(l.cpuMilli) / float64(in.TotalCPUMilli)
		}
		if in.TotalMemBytes > 0 {
			s = max(s, float64(l.memBytes)/float64(in.TotalMemBytes))
		}
		return s
	}

	var admit []types.NamespacedName
	for {
		var eligible []*lineState
		for _, l := range lines {
			if l.eligible() {
				eligible = append(eligible, l)
			}
		}
		slices.SortFunc(eligible, func(a, b *lineState) int {
			return cmp.Or(
				cmp.Compare(share(a), share(b)),
				a.queue[0].JoinedAt.Compare(b.queue[0].JoinedAt),
				cmp.Compare(a.name, b.name),
			)
		})
		admitted := false
		for _, l := range eligible {
			head := l.queue[0]
			if !in.Model.Reserve(head.Demand) {
				continue
			}
			cpu, mem := head.Demand.Total()
			l.cpuMilli += cpu
			l.memBytes += mem
			if !head.Child {
				l.nonChild++
			}
			l.queue = l.queue[1:]
			admit = append(admit, head.Key)
			admitted = true
			break
		}
		if !admitted {
			break
		}
	}

	positions := map[types.NamespacedName]Position{}
	for _, l := range lines {
		for i, c := range l.queue {
			positions[c.Key] = Position{Index: int32(i + 1), LineLength: int32(len(l.queue))}
		}
	}
	return Decision{Admit: admit, Positions: positions}
}
