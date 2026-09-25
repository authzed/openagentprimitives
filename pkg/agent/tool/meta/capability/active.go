package capability

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ActiveWithConfig resolves one capability's grant, active-decision, and
// parsed config in a single call, for consumers OUTSIDE the tool-assembly
// path (the runner's per-turn context contributions).
//
// It exists so those consumers cannot drift from Offer's view of the same
// capability: DefaultOn comes off the registered capability rather than being
// re-asserted at the call site, and the decision is agentcaps.Active rather than
// a hand-rolled equivalent. Unregistered, ungranted, disabled and misconfigured
// all report inactive.
//
// A malformed config returns (nil, false, err) — inactive AND an error. Callers
// MUST log it, never drop it.
func ActiveWithConfig(class *spiceboxv1alpha1.AgentClass, name string) (Config, bool, error) {
	c, ok := Lookup(name)
	if !ok {
		return nil, false, fmt.Errorf("capability %q is not registered", name)
	}
	grant, grantErr := agentcaps.GrantOf(class, name)
	if grantErr != nil {
		return nil, false, fmt.Errorf("capability %q: %w", name, grantErr)
	}
	if !agentcaps.Active(c.DefaultOn(), grant) {
		return nil, false, nil
	}
	cfg, err := c.ParseConfig(grant.Raw)
	if err != nil {
		return nil, false, err
	}
	return cfg, true, nil
}
