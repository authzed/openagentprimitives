package capability

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
)

func init() { Register(&userProfileCapability{}) }

// userProfileCapability gates whether the agent sees profile detail about the
// people it is talking to. Opt-in: absence of the key means no ambient block,
// no tool, and no API call.
//
// It contributes no tools in its first iteration — the deliverable is the
// per-turn ambient block the runner renders (pkg/agent/runner/speakerprofile.go),
// which consumes this capability's grant through ActiveWithConfig. The
// get_participant_profile tool lands here later, and will read the SAME
// parsed config.
type userProfileCapability struct{}

// UserProfileConfig is the parsed per-capability config. Fields is the
// operator's allowlist, already validated and defaulted by ParseConfig, so
// every consumer receives a usable list and none of them re-derives defaults.
type UserProfileConfig struct {
	Fields []userprofile.Field
}

// userProfileWire is the on-the-wire shape. Enabled is declared so the common
// {enabled} flag survives DisallowUnknownFields — an empty struct would
// wrongly reject a perfectly legal {"enabled": false}.
type userProfileWire struct {
	// Enabled is the common capability flag; nil means absent, hence enabled.
	Enabled *bool `json:"enabled,omitempty"`
	// Fields is the profile-field allowlist; absent means the default set.
	Fields []string `json:"fields,omitempty"`
}

func (userProfileCapability) Name() string          { return "user_profile" }
func (userProfileCapability) DefaultOn() bool       { return false }
func (userProfileCapability) Infrastructural() bool { return false }

// ParseConfig validates the allowlist and rejects anything it does not
// recognize. An unknown key (a typo like "feilds") or an unknown field name
// is an error, not a silently-ignorable extension: dropping it would expose a
// narrower profile than the operator configured, with no signal anywhere. The
// AgentClass controller surfaces the error as CapabilitiesValid.
func (userProfileCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	if len(raw) == 0 {
		return UserProfileConfig{Fields: userprofile.DefaultFields()}, nil
	}
	var wire userProfileWire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("user_profile: parsing capability config: %w", err)
	}
	fields, err := userprofile.ParseFields(wire.Fields)
	if err != nil {
		return nil, fmt.Errorf("user_profile: %w", err)
	}
	return UserProfileConfig{Fields: fields}, nil
}

// Offer contributes no tools yet. The ambient speaker block is rendered by the
// runner, not assembled here. Returning (nil, nil) rather than a SkipReason is
// deliberate — there is nothing wrong, there is simply no tool in this
// iteration, and a SkipReason would log a warning on every session.
func (userProfileCapability) Offer(OfferContext) ([]tool.Tool, *SkipReason) {
	return nil, nil
}
