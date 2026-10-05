package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func init() { Register(goalsCapability{}) }

type goalsCapability struct{}

func (goalsCapability) Name() string                                { return "goals" }
func (goalsCapability) DefaultOn() bool                             { return false }
func (goalsCapability) Infrastructural() bool                       { return false }
func (goalsCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }
func (goalsCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Session != nil && o.Session.Spec.Parent != nil {
		return nil, &SkipReason{Capability: "goals", Reason: "delegated children propose goal changes to their parent"}
	}
	if o.Class == nil || o.Class.Spec.GetAuthz().InformationLeakage.ResolvedMode() != "enforcing" {
		return nil, &SkipReason{Capability: "goals", Reason: "goals require enforcing information leakage policy"}
	}
	if o.Env.GoalsCaller == nil || !o.Env.MemoryAvailable {
		return nil, &SkipReason{Capability: "goals", Reason: "goal service unavailable"}
	}
	return meta.NewGoalTools(o.Env.GoalsCaller), nil
}
