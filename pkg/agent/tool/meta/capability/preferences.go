package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func init() { Register(&preferencesCapability{}) }

var _ SectionOfferer = preferencesCapability{}

// preferencesCapability contributes get_preferences and set_preference. It
// is DEFAULT-ON (unlike memory) because reading and saving a user's own
// per-user preferences is scoped to the current turn's author and to keys
// the class itself declared — there is nothing here for an operator to
// opt an agent into that the class hasn't already opted into by declaring
// spec.userPreferences.
//
// It contributes nothing (not a skip) when the class declares no
// preferences at all: a save/read surface with nothing to save or read is
// an absent feature, not an unavailable one. It skips (logged) only once
// the class HAS declared preferences but the runner has no reader/saver
// wired for this session — a wiring gap, not a class authoring choice.
type preferencesCapability struct{}

func (preferencesCapability) Name() string                                { return "preferences" }
func (preferencesCapability) DefaultOn() bool                             { return true }
func (preferencesCapability) Infrastructural() bool                       { return false }
func (preferencesCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

// Offer is the plain-Capability view of OfferWithSections: the same tools,
// the same skip, the section dropped. Assemble never calls it for this
// capability (it dispatches SectionOfferer), but a caller holding only a
// Capability must see exactly what Assemble would inject.
func (c preferencesCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	tools, _, skip := c.OfferWithSections(o)
	return tools, skip
}

// OfferWithSections gates both tools on the class having declared at least
// one preference AND the runner having a reader/saver wired for this
// session. The "guidance" prompt section rides the same decision: an agent
// with no preference tools gets no instruction to use them either.
func (preferencesCapability) OfferWithSections(o OfferContext) ([]tool.Tool, []PromptSection, *SkipReason) {
	if len(o.Env.UserPreferences) == 0 {
		return nil, nil, nil // class declares none → feature absent, not a skip
	}
	if o.Env.PreferencesReader == nil || o.Env.PreferenceSaver == nil {
		return nil, nil, &SkipReason{Capability: "preferences", Reason: "preferences service not available"}
	}
	tools := []tool.Tool{
		meta.NewGetPreferences(o.Env.PreferencesReader),
		meta.NewSetPreference(o.Env.PreferencesReader, o.Env.PreferenceSaver),
	}
	return tools, []PromptSection{preferencesSection}, nil
}

// preferencesSectionTitle heads the prompt section this capability
// contributes. Kept as a named constant for the same reason
// pageSectionTitle is: a bundle or test can assert on it verbatim without
// restating the body.
const preferencesSectionTitle = "Per-user preferences"

var preferencesSection = PromptSection{
	Title: preferencesSectionTitle,
	Body: "this agent offers per-user preferences (get_preferences lists them with the current user's values). " +
		"When the user expresses a durable preference that matches a declared key, offer to save it with " +
		"set_preference — every save asks the user to confirm first. Read preferences before assuming defaults " +
		"when personalization matters. get_preferences can also read a NAMED user's preferences (for example " +
		"the trigger author, on a session opened by a webhook) — only the keys the class marks " +
		"visibility: class are shared for anyone other than the current user.",
}
