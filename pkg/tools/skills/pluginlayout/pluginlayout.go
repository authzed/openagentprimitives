// Package pluginlayout is the on-disk contract for the plugin a staged skill
// was discovered inside.
//
// Some skills are published as one component of a Claude Code plugin
// (<plugin>/.claude-plugin/plugin.json, <plugin>/skills/<name>/SKILL.md,
// <plugin>/agents/*.md), and their instructions dispatch the plugin's own
// subagents by name ("launch the bug-hunter agent",
// "differential-review:adversarial-modeler"). A skill staged on its own
// carries none of those agents, so the dispatch has nothing to land on and the
// methodology silently degrades to whatever the consumer improvises.
//
// Discovery (pkg/controllers/skillsource) records the plugin's name and its
// agents on Skill.spec.plugin; staging (pkg/controllers/agentsession) writes
// them into the skill's staged directory under Dir; and a disk-based consumer
// (internal/cmd/claudeshim, via pkg/tools/toolchain/claude) reassembles a
// plugin from them. This package is the one place all three agree on paths
// and names, and it stays stdlib-only so the sandbox-side shim can import it.
//
// Only the plugin's NAME and its agents/*.md travel. Hooks, MCP servers,
// commands, settings and the plugin's other skills never do: hooks run
// arbitrary shell at session events and MCP servers start processes with
// their own egress, either of which would sidestep the sandbox's toolspec
// gates. Agent files are markdown prompts — the same trust class as the
// SKILL.md body that already travels.
package pluginlayout

import (
	"path"
	"regexp"
	"strings"
)

const (
	// Dir is the reserved directory inside a staged skill bundle that carries
	// the skill's plugin context. Discovery and staging both refuse to let a
	// skill's own bundle files occupy it, so its contents always come from
	// Skill.spec.plugin and nowhere else.
	Dir = ".oap-plugin"
	// ManifestFile is the plugin manifest staged under Dir. AP writes it
	// itself, carrying only the plugin name; an upstream plugin.json is never
	// copied, so a manifest-declared hook or MCP server cannot ride along.
	ManifestFile = "plugin.json"
	// AgentsDir is the directory under Dir holding one <name>.md per agent.
	AgentsDir = "agents"
	// AgentExt is the file extension of an agent definition.
	AgentExt = ".md"

	// UpstreamManifestPath is where a Claude Code plugin keeps its manifest,
	// relative to the plugin root.
	UpstreamManifestPath = ".claude-plugin/plugin.json"
	// UpstreamSkillsDir is the plugin-root directory whose children are skill
	// directories.
	UpstreamSkillsDir = "skills"
	// UpstreamAgentsDir is the plugin-root directory whose direct *.md
	// children are agent definitions.
	UpstreamAgentsDir = "agents"
)

var (
	// nameRe matches a plugin name: the kebab-case shape Claude Code requires
	// of plugin names, and the same shape validate requires of skill names.
	// It becomes a directory name and every agent's namespace prefix.
	nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// agentNameRe matches an agent file stem. No dots and no separators, so a
	// name can never climb out of AgentsDir.
	agentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// Manifest is the subset of plugin.json AP reads and writes.
type Manifest struct {
	Name string `json:"name"`
}

// MaxNameLen bounds a plugin name and an agent name alike.
const MaxNameLen = 64

// ValidName reports whether s is an acceptable plugin name.
func ValidName(s string) bool {
	return len(s) <= MaxNameLen && nameRe.MatchString(s)
}

// ValidAgentName reports whether s is an acceptable agent file stem.
func ValidAgentName(s string) bool {
	return len(s) <= MaxNameLen && agentNameRe.MatchString(s)
}

// ManifestPath is the manifest's path relative to a staged skill directory.
func ManifestPath() string { return path.Join(Dir, ManifestFile) }

// AgentPath is an agent's path relative to a staged skill directory.
func AgentPath(name string) string { return path.Join(Dir, AgentsDir, name+AgentExt) }

// IsReserved reports whether rel, a path relative to a skill directory, falls
// inside Dir.
func IsReserved(rel string) bool {
	return rel == Dir || strings.HasPrefix(rel, Dir+"/")
}
