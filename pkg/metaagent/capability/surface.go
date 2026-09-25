package capability

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
)

// PermissionChecker answers whether the SPEAKER holds a SpiceDB permission on
// the session.
//
// A resolved function rather than a client, for the same reason Env carries
// funcs: the surface builder is handed the exact question it may ask, and
// nothing it could use to ask a different one.
type PermissionChecker func(ctx context.Context, permission string) (bool, error)

// Skipped is one capability that was NOT offered, and why.
//
// Exclusions are returned rather than dropped because a user whose utterance
// does nothing deserves to know which of the several possible reasons applied.
// "Nothing happened" is the failure mode that makes people conclude a feature is
// broken and stop using it, and the reasons point at different fixes: a standing
// problem sends them to the session owner, an admin switch-off sends them to an
// administrator, and a lookup failure is nobody's fault but needs reporting.
type Skipped struct {
	Capability string
	Reason     string
}

// SurfaceFor computes the actions this speaker may invoke.
//
// Three independent gates, each with a distinct exclusion reason:
//
//   - ENABLEMENT. A default-off capability is offered only when explicitly
//     listed. `budget` and `model` are off by default, so registering them does
//     not make them reachable.
//   - STANDING. The speaker must hold the capability's Permission() on the
//     session. This is what stops a thread participant from cancelling
//     everyone's work.
//   - ADMIN CEILING. Permitted=false refuses regardless of standing — `model`
//     with allowModelOverride unset. Standing says WHO may ask; the ceiling says
//     WHETHER anyone may.
//
// A standing lookup that ERRORS excludes the capability rather than guessing.
// Treating an unreachable SpiceDB as "they probably hold it" would offer actions
// nobody verified they may invoke — and the surface is what the classifier is
// then allowed to choose from, so a wrong answer here widens the whole module's
// reach. One capability's failure does not fail the surface, though: the others
// are still legitimately answerable, and losing every capability because one
// lookup timed out would be its own outage.
func SurfaceFor(
	ctx context.Context,
	state metaagent.StateSnapshot,
	holds PermissionChecker,
	enabled []string,
) ([]metaagent.Action, []Skipped, error) {
	if holds == nil {
		// No way to establish standing means no capability can be offered.
		// Failing closed here is the difference between "offered nothing" and
		// "offered everything to anyone".
		return nil, nil, fmt.Errorf(
			"metaagent: no permission checker wired; refusing to compute a capability " +
				"surface nobody's standing was verified against")
	}

	enabledSet := make(map[string]struct{}, len(enabled))
	for _, e := range enabled {
		enabledSet[e] = struct{}{}
	}

	var (
		actions []metaagent.Action
		skipped []Skipped
	)
	for _, c := range All() {
		if _, on := enabledSet[c.Name()]; !on && !c.DefaultOn() {
			// Not an exclusion worth reporting: the capability was never asked
			// for. Reporting it would bury the reasons that DO need reading.
			continue
		}

		if b := c.Ceiling(state); !b.Permitted {
			skipped = append(skipped, Skipped{
				Capability: c.Name(),
				Reason: "an administrator has switched this capability off for this session; " +
					"no standing makes it available",
			})
			continue
		}

		ok, err := holds(ctx, c.Permission())
		if err != nil {
			skipped = append(skipped, Skipped{
				Capability: c.Name(),
				Reason:     fmt.Sprintf("could not establish standing: %v", err),
			})
			continue
		}
		if !ok {
			skipped = append(skipped, Skipped{
				Capability: c.Name(),
				Reason: fmt.Sprintf("you do not hold %s on this session; ask the session owner",
					c.Permission()),
			})
			continue
		}

		actions = append(actions, c.Actions()...)
	}
	return actions, skipped, nil
}
