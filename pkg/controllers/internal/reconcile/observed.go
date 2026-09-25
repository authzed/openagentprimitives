package reconcile

import (
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StampIfMoved maintains a wall-clock observation field on a controller-owned
// status — `lastValidatedAt` and friends — with set-on-meaningful-change
// semantics (AGENTS.md §Server-side apply: "preserve the existing value when
// the inputs that define it are unchanged, update only when they actually
// change").
//
// `before` is the status as it was read, `after` the status the phase chain
// produced, and `field` points at the observation field inside it. Callers seed
// that field with its prior value before calling, so the field cannot make
// itself look changed; StampIfMoved then bumps it only when something else in
// status actually moved (or when it was never set).
//
// This matters more than the timestamp's accuracy. RunPhases persists status on
// every pass, and controllers here register `For(...)` with no predicate, so
// they see their own writes. When every field but the clock is stable, holding
// the clock still makes the write byte-identical, the API server short-circuits
// it, and no watch event is produced. Re-stamping unconditionally instead turns
// each no-op reconcile into a real write that re-enqueues the controller — a
// self-sustaining loop for any CR whose reconcile cycle spans a second, which a
// slow or blackholed probe endpoint guarantees.
func StampIfMoved[S any](before, after S, field **metav1.Time) {
	if *field != nil && equality.Semantic.DeepEqual(before, after) {
		return
	}
	now := metav1.Now()
	*field = &now
}
