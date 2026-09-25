// Package agentcaps resolves AgentClass capability grants.
//
// Kept tiny and dependency-free (only pkg/apis/v1alpha1) so any package can
// import it without pulling in the runner's tool/modality/channelassets graph.
// There must be exactly ONE definition of the grant/active semantics: parallel
// copies drift, and a drift means a capability reads enabled to one component
// and disabled to another.
//
// What agentcaps must NOT become: the general capability CONTRACT
// (Name/DefaultOn/ParseConfig/Offer) belongs to pkg/agent/tool/meta/capability,
// and moving it here would drag the tool graph in and defeat the point. Only
// resolution logic genuinely shared across the tool boundary lands here.
//
// Read from spec, never from status: status is eventually consistent, so a
// revocation would leave a window in which a status reader still allows. An
// authz input must not fail open on staleness.
package agentcaps

import (
	"encoding/json"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Grant is the resolved state of one capability key on an AgentClass.
type Grant struct {
	// Granted reports whether the key is present in spec.capabilities.
	Granted bool
	// Enabled is the common {enabled} flag. Absent ⇒ true. Malformed ⇒ false.
	Enabled bool
	// Raw is the unparsed value, for the capability's own ParseConfig.
	Raw json.RawMessage
}

// commonConfig is the enable/disable field shared by every capability value.
type commonConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// EnabledFrom extracts the common {enabled} flag from a raw capability config
// value. Absent/nil raw or an absent field defaults to true.
//
// A malformed value is FAIL CLOSED: enabled=false AND a non-nil error. Callers
// must log the error — never drop it (AGENTS.md: never silently drop errors).
func EnabledFrom(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return true, nil
	}
	var cc commonConfig
	if err := json.Unmarshal(raw, &cc); err != nil {
		return false, err
	}
	if cc.Enabled == nil {
		return true, nil
	}
	return *cc.Enabled, nil
}

// GrantOf reads the AgentClass capabilities map for one capability name.
// A nil class, a nil map, or an absent key all yield the zero Grant.
func GrantOf(class *spiceboxv1alpha1.AgentClass, name string) (Grant, error) {
	if class == nil || class.Spec.Capabilities == nil {
		return Grant{}, nil
	}
	v, ok := class.Spec.Capabilities[name]
	if !ok {
		return Grant{}, nil
	}
	enabled, err := EnabledFrom(v.Raw)
	return Grant{Granted: true, Enabled: enabled, Raw: v.Raw}, err
}

// Active decides whether a capability is active given its DefaultOn property
// and its resolved Grant.
//
//	defaultOn:  absent → on;  present → enabled != false
//	opt-in:     present AND enabled != false
func Active(defaultOn bool, g Grant) bool {
	if defaultOn {
		return !g.Granted || g.Enabled
	}
	return g.Granted && g.Enabled
}
