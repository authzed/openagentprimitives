package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func init() { Register(&sessionViewsCapability{}) }

// sessionViewsCapability is OPT-IN (default-off). It grants a browser VIEW of a
// session the right to show a transcript and (per {interactions}) to talk back.
// It offers NO agent tools — it gates a browser affordance, resolved directly by
// webd and pkg/web/webui/interact via agentcaps.ResolveSessionViews. Registered so
// the AgentClass controller's CapabilitiesValid accepts the key and validates
// {interactions}.
//
// The config type and its ABSENT-⇒-NONE semantics live in agentcaps rather than
// here, because three components read this value and agentcaps is the only
// package all three can import. DefaultOn() below and agentcaps'
// sessionViewsDefaultOn must agree.
type sessionViewsCapability struct{}

func (sessionViewsCapability) Name() string          { return agentcaps.SessionViewsCapability }
func (sessionViewsCapability) DefaultOn() bool       { return false }
func (sessionViewsCapability) Infrastructural() bool { return false }

func (sessionViewsCapability) ParseConfig(raw json.RawMessage) (Config, error) {
	cfg, err := agentcaps.ParseSessionViewsConfig(raw)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// Offer returns no tools — session_views is a browser affordance, not a tool.
func (sessionViewsCapability) Offer(OfferContext) ([]tool.Tool, *SkipReason) { return nil, nil }
