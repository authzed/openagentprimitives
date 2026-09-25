package pipeline

import "sort"

type entry struct {
	hook  Hook
	order int
}

// Registry maps lifecycle points to the hooks registered there, kept sorted by
// each hook's code-owned order. Construct fresh in tests with NewRegistry; use
// the package-global Default for production init() registration.
type Registry struct {
	byPoint map[Point][]entry
}

func NewRegistry() *Registry { return &Registry{byPoint: map[Point][]entry{}} }

// Default is the process-global registry production hooks register into.
var Default = NewRegistry()

// Register adds h at each of its Points with the given order (lower runs first).
func (r *Registry) Register(h Hook, order int) {
	for _, p := range h.Points() {
		r.byPoint[p] = append(r.byPoint[p], entry{hook: h, order: order})
		// Re-sorted on each insert; hooks-per-point is tiny, so the O(n^2) is irrelevant.
		sort.SliceStable(r.byPoint[p], func(i, j int) bool {
			return r.byPoint[p][i].order < r.byPoint[p][j].order
		})
	}
}

// Hooks returns the hooks registered at p, ascending by order (stable for ties).
func (r *Registry) Hooks(p Point) []Hook {
	es := r.byPoint[p]
	out := make([]Hook, len(es))
	for i, e := range es {
		out[i] = e.hook
	}
	return out
}

// Register adds h to the Default registry. For production init() use.
func Register(h Hook, order int) { Default.Register(h, order) }
