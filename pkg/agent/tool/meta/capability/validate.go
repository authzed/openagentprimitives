package capability

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
)

// ErrUnknownCapability is the sentinel wrapped by ValidateGrant when a
// spec.capabilities key names no registered capability. Use
// IsUnknownCapability (or errors.Is against this value) to distinguish that
// failure class from a config-parse failure.
var ErrUnknownCapability = errors.New("unknown capability")

// ValidateGrant statically validates one spec.capabilities entry: name must be a
// registered capability, and raw must parse both as the common {enabled}
// envelope and as that capability's own ParseConfig. Pure — no RunnerEnv — so it
// is safe from a controller reconcile loop.
//
// An error wrapping ErrUnknownCapability means the name is not registered; any
// other error is a config-parse failure. IsUnknownCapability tells them apart.
func ValidateGrant(name string, raw json.RawMessage) error {
	c, ok := Lookup(name)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownCapability, name)
	}
	// agentcaps.EnabledFrom is the same common-envelope parse Assemble runs at
	// runtime (via agentcaps.GrantOf); check it here too so a malformed
	// {enabled} field is caught even for capabilities whose ParseConfig is a
	// no-op (e.g. memory) and would otherwise never see the bad JSON.
	if _, err := agentcaps.EnabledFrom(raw); err != nil {
		return fmt.Errorf("capability %q: invalid config: %w", name, err)
	}
	if _, err := c.ParseConfig(raw); err != nil {
		return fmt.Errorf("capability %q: invalid config: %w", name, err)
	}
	return nil
}

// IsUnknownCapability reports whether err (as returned by ValidateGrant) is
// the "name not registered" failure class, as opposed to a config-parse
// failure.
func IsUnknownCapability(err error) bool {
	return errors.Is(err, ErrUnknownCapability)
}
