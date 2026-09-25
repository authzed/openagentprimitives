package capability

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
)

// Outcome is what should happen with a classified decision.
//
// Exactly one of Apply, RequiresApproval or Refused is meaningful, and the
// three are kept separate rather than collapsed into a verdict enum because
// they need different handling: Apply acts now, RequiresApproval publishes a
// card and waits, and Refused surfaces a notice to the speaker — a no-op with
// an explanation, never a silent drop.
type Outcome struct {
	// Apply means effect it now: an offered narrowing.
	Apply bool
	// RequiresApproval means publish a card and wait: an offered widening.
	RequiresApproval bool
	// Refused is why nothing will happen. Non-empty means refused, and the text
	// is user-facing.
	Refused string
}

// Resolve decides what to do with a classified decision.
//
// # The bypass this closes
//
// The Surface is STANDING-GATED — SurfaceFor already excluded every capability
// the speaker lacks permission for, and every one an administrator switched
// off. If a decision naming an action that is not on that surface were applied,
// the standing gate would be bypassed entirely: a classifier that hallucinated,
// or was steered into, "lifecycle/cancel_session" would cancel a session for a
// speaker who was never offered it.
//
// So membership in the surface is checked, not assumed. The model picks FROM
// the surface; it does not get to name something outside it.
//
// # Direction is re-resolved, not read
//
// Even the matching surface entry's own Direction is ignored, and DirectionOf
// consults the registry instead. The surface is runtime-built today, but it
// travels inside Input alongside the untrusted turn, and having exactly ONE
// place that decides what an action costs is worth more than an assumption
// about which field was built by whom. It is the same reasoning that keeps
// Direction off Decision entirely.
func Resolve(in metaagent.Input, d metaagent.Decision) (Outcome, error) {
	if !offered(in.Surface, d) {
		return Outcome{
			Refused: fmt.Sprintf(
				"%s/%s was not offered for this session, so nothing was done. "+
					"That is usually because you do not hold the permission it needs, "+
					"or an administrator has switched it off.",
				d.Capability, d.Action),
		}, nil
	}

	// From the REGISTRY, never from the surface entry or the decision.
	dir, err := DirectionOf(d)
	if err != nil {
		return Outcome{}, err
	}
	if dir == metaagent.Widening {
		return Outcome{RequiresApproval: true}, nil
	}
	return Outcome{Apply: true}, nil
}

// offered reports whether this exact capability+action pair is on the surface.
func offered(surface []metaagent.Action, d metaagent.Decision) bool {
	for _, a := range surface {
		if a.Capability == d.Capability && a.Name == d.Action {
			return true
		}
	}
	return false
}
