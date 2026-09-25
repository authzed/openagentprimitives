package capability

import (
	"fmt"
	"strings"
)

// ClampNumeric bounds a requested value to the administrative ceiling.
//
// The ceiling rule, mechanised: a capability moves WITHIN a bound an
// administrator already set, and never raises one. Clamping happens BEFORE the
// approval card is rendered, so a human is never shown — and therefore can never
// grant — a value outside the admin bound. A clamped request renders honestly:
// "requested 2M tokens; the ceiling for this class is 500k — approve 500k?"
//
// The consequence worth stating: the worst case for a fully compromised
// metaagent is a session using authority an administrator had ALREADY
// sanctioned, with a human approving the move. It cannot manufacture new
// authority.
//
// Returns the granted value, whether it was cut, and an error when the request
// cannot be honoured at all.
func ClampNumeric(requested int64, b Bound) (granted int64, clamped bool, err error) {
	if !b.Permitted {
		return 0, false, fmt.Errorf(
			"metaagent: this capability is not permitted for this session; " +
				"an administrator has switched it off, so no standing makes it available")
	}
	if b.Max <= 0 {
		// An undeclared ceiling is NOT "unlimited". A capability that forgot to
		// declare its bound must refuse rather than wave everything through: the
		// failure direction has to be friction an operator notices, never silent
		// permission, which is the same reasoning tier-0 auto-approval follows
		// for an unset budget.
		return 0, false, fmt.Errorf(
			"metaagent: this capability declares no ceiling, so nothing can be granted " +
				"against it; the bound has to be set before the capability can move within it")
	}
	if b.Min > 0 && requested < b.Min {
		// REFUSED, not raised to the floor. Raising would grant more than was
		// asked for, turning a narrowing utterance into a widening outcome —
		// the one direction that must never happen without approval. Refusing is
		// honest and cannot surprise anyone.
		return 0, false, fmt.Errorf(
			"metaagent: %d is below the floor of %d an administrator set for this capability; "+
				"refusing rather than silently granting more than was asked for",
			requested, b.Min)
	}
	if requested > b.Max {
		return b.Max, true, nil
	}
	return requested, false, nil
}

// ClampChoice bounds a requested value to an enumerated ceiling — a model name
// against the catalog, say.
//
// A value outside the set is REFUSED, never substituted. Picking the "closest"
// allowed value would silently run the session on something nobody chose, and
// the user would have no way to tell it happened.
func ClampChoice(requested string, b Bound) (string, error) {
	if !b.Permitted {
		return "", fmt.Errorf(
			"metaagent: this capability is not permitted for this session; " +
				"an administrator has switched it off, so no standing makes it available")
	}
	if len(b.Allowed) == 0 {
		return "", fmt.Errorf(
			"metaagent: this capability declares no allowed values, so nothing can be " +
				"granted against it")
	}
	for _, a := range b.Allowed {
		if a == requested {
			return requested, nil
		}
	}
	return "", fmt.Errorf(
		"metaagent: %q is not among the values an administrator allows (%s); "+
			"refusing rather than substituting a value nobody chose",
		requested, strings.Join(b.Allowed, ", "))
}
