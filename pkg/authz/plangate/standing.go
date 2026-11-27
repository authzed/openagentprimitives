package plangate

// CoveredByStanding reports whether a scheduled run's freshly-made plan widens
// nothing relative to the plan a manager approved once (the standing plan), so
// the run may be auto-approved.
//
// Phases match by position, the same identity DiffPlans uses. The run may have
// FEWER phases than the standing plan (dropping reach never needs consent) but
// never more: an extra phase is entirely new authority. Each phase pair must
// not Widen under DiffPhase, which covers every digest dimension.
//
// A run with zero phases is NOT covered. It has nothing to approve and the gate
// never asks for it, so vacuous truth would only be a way to auto-approve a
// malformed or empty plan; false is the fail-closed reading.
func CoveredByStanding(standing, run Plan) bool {
	if len(run.Phases) == 0 || len(run.Phases) > len(standing.Phases) {
		return false
	}
	for i, ph := range run.Phases {
		if d, changed := DiffPhase(standing.Phases[i], ph, i); changed && d.Widens() {
			return false
		}
	}
	return true
}
