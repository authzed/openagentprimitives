package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func init() { Register(&knowledgeCapability{}) }

// knowledgeCapability is opt-in (must be explicitly granted) and contributes
// query_knowledge. It skips (no tools, logged SkipReason) when no knowledge
// graph endpoint is configured.
type knowledgeCapability struct{}

func (knowledgeCapability) Name() string                                { return "knowledge" }
func (knowledgeCapability) DefaultOn() bool                             { return false }
func (knowledgeCapability) Infrastructural() bool                       { return false }
func (knowledgeCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (knowledgeCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if !o.Env.KGAvailable {
		return nil, &SkipReason{Capability: "knowledge", Reason: "no knowledge-graph endpoint available"}
	}
	return []tool.Tool{meta.NewQueryKnowledge()}, nil
}
