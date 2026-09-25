package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func init() { Register(&memoryCapability{}) }

// memoryCapability is opt-in (must be explicitly granted) and contributes
// query_memory, plus search_memory when a search provider is present, plus
// record_observation unconditionally once a memory backend is wired. It
// skips (no tools, logged SkipReason) when no memory backend is wired.
type memoryCapability struct{}

func (memoryCapability) Name() string                                { return "memory" }
func (memoryCapability) DefaultOn() bool                             { return false }
func (memoryCapability) Infrastructural() bool                       { return false }
func (memoryCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil } // room to grow (maxResults, …)

func (memoryCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if !o.Env.MemoryAvailable {
		return nil, &SkipReason{Capability: "memory", Reason: "no memory backend available"}
	}
	tools := []tool.Tool{meta.NewQueryMemory()}
	if o.Env.SearchAvailable {
		tools = append(tools, meta.NewSearchMemory())
	}
	// record_observation is offered whenever a memory backend is wired at
	// all — it needs no search provider, only somewhere to write. Still
	// gated by this capability being GRANTED (DefaultOn is false, see
	// memory_test.go): a write tool must not become ambient.
	tools = append(tools, meta.NewRecordObservation())
	return tools, nil
}
