package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func init() { Register(&agentBuilderCapability{}) }

// agentBuilderCapability is the DECLARATION half of the agent-builder
// workshop (spec §1.1). Granting it on an AgentClass sanctions nothing by
// itself: the sanction lives operator-side, in
// ClusterAgentSettings.Limits.BuilderClasses, checked by
// agentsession.ensureWorkshop against the class's (namespace, name) AND its
// own reference to the named SidecarToolbox. This capability exists only so
// a class can opt in to being considered at all — an unsanctioned class
// granting it gets no workshop tools, exactly like one that never grants it.
//
// Opt-in (DefaultOn == false): a class must ask, the same posture as every
// other capability that can put new surface in front of an agent.
type agentBuilderCapability struct{}

func (agentBuilderCapability) Name() string                                { return "agent_builder" }
func (agentBuilderCapability) DefaultOn() bool                             { return false }
func (agentBuilderCapability) Infrastructural() bool                       { return false }
func (agentBuilderCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

// SanctionGated marks agent_builder as the one capability whose actual grant
// is decided outside the AgentClass's own spec: declaring it here is
// necessary but not sufficient, since the operator-side sanction is what
// ships the workshop tools (via the sidecar, not via this capability).
// TestAgentBuilderIsTheOnlySanctionGatedCapability walks the live registry
// and fails by name if a second capability ever claims this, or if this one
// loses the marker in a refactor.
func (agentBuilderCapability) SanctionGated() bool { return true }

// Offer never contributes a tool: the workshop's tools ship with the
// workshop sidecar itself (a SidecarToolbox, wired by ensureWorkshop once
// sanctioned), not through the meta-tool assembly path this capability
// participates in. Offer's only job is to report, when granted, that the
// grant alone did not sanction anything — so an operator reading the skip
// log does not mistake a granted-but-inert capability for a working one.
func (agentBuilderCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if !o.Granted {
		return nil, nil
	}
	return nil, &SkipReason{
		Capability: "agent_builder",
		Reason:     "workshop tools ship with the workshop sidecar; declaring the capability sanctions nothing by itself",
	}
}
