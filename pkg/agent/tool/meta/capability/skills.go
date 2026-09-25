package capability

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func init() { Register(&skillsCapability{}) }

// skillsCapability is default-on and contributes load_skill, but only when the
// AgentClass opted into at least one skill whose SKILL.md body resolved
// (RunnerEnv.SkillBodies non-empty). With no resolved bodies there is nothing
// to load, so Offer is inactive — returns (nil, nil) rather than a SkipReason:
// an agent with no skills is a normal state, not a granted-but-unavailable one.
// Ports internal/cmd/runner/main.go:1225-1229.
type skillsCapability struct{}

func (skillsCapability) Name() string                                { return "skills" }
func (skillsCapability) DefaultOn() bool                             { return true }
func (skillsCapability) Infrastructural() bool                       { return false }
func (skillsCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

func (skillsCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if len(o.Env.SkillBodies) == 0 {
		return nil, nil // no resolved skill bodies → nothing to load, inactive
	}
	return []tool.Tool{meta.NewLoadSkill(o.Env.SkillBodies)}, nil
}
