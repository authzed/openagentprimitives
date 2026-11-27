package admission

import (
	"cmp"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// WarmIdle is an Idle session whose pods still hold capacity and could be
// reclaimed early.
type WarmIdle struct {
	Key       types.NamespacedName
	Line      string
	IdleSince time.Time
	Pods      []PlacedPod
	// Child is set for a delegated child session, which never counts toward
	// its line's cap and so frees no cap slot.
	Child bool
}

// PlanReclaim picks which warm Idle sessions of the head's own line to
// reclaim so the head fits. Only sessions idle at least minWarm are eligible,
// oldest first (ties by key), freed one at a time on a clone of model until
// the head's demand reserves; the freed prefix is returned. It returns nil
// when the model is unknown, when the head already fits, or when freeing every
// eligible session still does not fit. The caller's model is never mutated.
func PlanReclaim(head Candidate, warm []WarmIdle, model CapacityModel, now time.Time, minWarm time.Duration) []types.NamespacedName {
	if !model.Known() || model.Clone().Reserve(head.Demand) {
		return nil
	}
	var eligible []WarmIdle
	for _, w := range warm {
		if w.Line == head.Line && now.Sub(w.IdleSince) >= minWarm {
			eligible = append(eligible, w)
		}
	}
	slices.SortFunc(eligible, func(a, b WarmIdle) int {
		return cmp.Or(a.IdleSince.Compare(b.IdleSince), cmp.Compare(a.Key.String(), b.Key.String()))
	})
	scratch := model.Clone()
	var plan []types.NamespacedName
	for _, w := range eligible {
		scratch.Free(w.Pods)
		plan = append(plan, w.Key)
		if scratch.Reserve(head.Demand) {
			return plan
		}
	}
	return nil
}

// PlanCapReclaim picks the warm Idle session to reclaim for a head whose line
// is at its cap (Decision.CapBlocked): the head needs a slot, not space, and
// one reclaimed non-child holder gives it one. Of head's line's non-child
// warm sessions idle at least minWarm, the oldest (ties by key) is returned.
// ok is false for a child head (a cap never blocks it) or when none is
// eligible.
func PlanCapReclaim(head Candidate, warm []WarmIdle, now time.Time, minWarm time.Duration) (types.NamespacedName, bool) {
	if head.Child {
		return types.NamespacedName{}, false
	}
	var pick *WarmIdle
	for i := range warm {
		w := &warm[i]
		if w.Line != head.Line || w.Child || now.Sub(w.IdleSince) < minWarm {
			continue
		}
		if pick == nil || cmp.Or(w.IdleSince.Compare(pick.IdleSince), cmp.Compare(w.Key.String(), pick.Key.String())) < 0 {
			pick = w
		}
	}
	if pick == nil {
		return types.NamespacedName{}, false
	}
	return pick.Key, true
}
