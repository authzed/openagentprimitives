// pkg/agent/tool/meta/load_skill.go
//
// load_skill implements the on-demand half of agent-skill progressive
// disclosure. Each opted-in skill's name+description is always in the system
// prompt (cheap); when the model decides a skill is relevant it calls load_skill
// with the canonical name to pull the full SKILL.md body into context. Bodies
// are loaded once at runner startup and closed over here.
package meta

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

type loadSkillTool struct {
	bodies map[string]string // canonical name → SKILL.md body
}

// NewLoadSkill builds the tool over the resolved skill bodies. Append it to the
// runner's merged tool list (it is NOT auto-registered — it needs runtime data,
// like meta.NewIntrospect).
func NewLoadSkill(bodies map[string]string) tool.Tool {
	return &loadSkillTool{bodies: bodies}
}

func (*loadSkillTool) Name() string    { return "load_skill" }
func (*loadSkillTool) Kind() tool.Kind { return tool.KindMeta }

func (*loadSkillTool) Description() string {
	return "Load the full instructions for an agent skill by its canonical name. " +
		"The available skills and when to use each are listed in the Agent Skills " +
		"section of your system prompt; call this only when a skill is relevant."
}

func (*loadSkillTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"skill": {
				"type": "string",
				"description": "The canonical name of the skill to load (as listed in the Agent Skills section)."
			}
		},
		"required": ["skill"]
	}`)
}

func (t *loadSkillTool) Execute(_ context.Context, args json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var in struct {
		Skill string `json:"skill"`
	}
	if res, ok := tool.ParseArgs(args, &in, t.Name(), `{"skill": "github.com/org/repo//skills/x@v1.2.0"}`); !ok {
		return res, nil
	}
	name := strings.TrimSpace(in.Skill)
	body, ok := t.bodies[name]
	if !ok {
		// Platform-authored: this package writes the whole string (the model's
		// own echoed name plus the available-skill list). Keeping it Trusted is
		// what guarantees delivery — a withheld "unknown skill" leaves the model
		// unable to learn the correct name and it just calls again.
		return tool.Result{Content: t.unknown(name), IsError: true, Trusted: true}, nil
	}
	// Trusted is deliberately NOT set. Result.Trusted means the content is
	// framework-CONTROLLED, not that a framework tool returned it. A SKILL.md
	// body is fetched from whatever git repo and ref a SkillSource names (an
	// empty ref resolves to the default BRANCH), so whoever can push there
	// authors this text — and a skill body is, by design, instructions the model
	// is told to follow. Leaving Trusted false routes it through
	// Loop.inspectUntrustedResult, the only content inspection a meta tool gets
	// (meta tools bypass the PostToolCall pipeline entirely).
	return tool.Result{Content: body}, nil
}

func (t *loadSkillTool) unknown(name string) string {
	avail := make([]string, 0, len(t.bodies))
	for n := range t.bodies {
		avail = append(avail, n)
	}
	sort.Strings(avail)
	if len(avail) == 0 {
		return "load_skill: no skills are available to this agent."
	}
	return "load_skill: unknown skill " + name + ". Available skills:\n  " + strings.Join(avail, "\n  ")
}

func (*loadSkillTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*loadSkillTool) PermissionVariants() []authz.PermissionVariant { return nil }
