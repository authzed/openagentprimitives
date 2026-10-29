// pkg/controllers/agentsession/skills_plugin.go
//
// Staging of a skill's plugin context (Skill.spec.plugin) into the reserved
// pluginlayout.Dir of its staged directory. See pkg/tools/skills/pluginlayout
// for the contract and for why only the plugin's name and agents travel.
package agentsession

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/pluginlayout"
)

// strippedAgentFrontmatter are the agent frontmatter fields that would give
// an agent definition reach beyond a prompt: its own lifecycle hooks (shell),
// its own MCP servers (processes and egress), and a permission mode that
// loosens the consumer's approval policy. Claude Code already ignores all
// three on agents loaded from a plugin; staging removes them anyway so that
// guarantee does not depend on which consumer, or which version of it, reads
// the file.
var strippedAgentFrontmatter = []string{"hooks", "mcpServers", "permissionMode"}

// pluginFiles renders p into the files staged under pluginlayout.Dir, keyed
// relative to the skill directory: an AP-written manifest carrying only the
// name, and one sanitized file per agent. An agent that cannot be staged
// safely is left out and described in skipped; a plugin whose name is invalid
// stages nothing at all (the skill then stages standalone). Pure function.
func pluginFiles(p *spiceboxv1alpha1.SkillPlugin) (files map[string][]byte, skipped []string) {
	if p == nil {
		return nil, nil
	}
	if !pluginlayout.ValidName(p.Name) {
		return nil, []string{fmt.Sprintf("plugin name %q is invalid; staging the skill standalone", p.Name)}
	}
	manifest, err := json.Marshal(pluginlayout.Manifest{Name: p.Name})
	if err != nil {
		return nil, []string{fmt.Sprintf("marshal plugin manifest: %v", err)}
	}
	files = map[string][]byte{pluginlayout.ManifestPath(): manifest}
	for _, a := range p.Agents {
		if !pluginlayout.ValidAgentName(a.Name) {
			skipped = append(skipped, fmt.Sprintf("agent %q: invalid agent name", a.Name))
			continue
		}
		content, err := stripAgentFrontmatter(a.Content)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("agent %q: %v", a.Name, err))
			continue
		}
		files[pluginlayout.AgentPath(a.Name)] = []byte(content)
	}
	return files, skipped
}

// pluginFingerprint is a cheap digest of everything pluginFiles derives its
// output from, folded into the staging sourceDigest so an agent edit upstream
// re-stages the skill. "" when there is no plugin, which keeps the
// fingerprint of every standalone skill exactly what it was before plugins
// were staged at all.
func pluginFingerprint(p *spiceboxv1alpha1.SkillPlugin) string {
	if p == nil {
		return ""
	}
	b, err := json.Marshal(p)
	if err != nil {
		// A struct of strings cannot fail to marshal; fall back to the name so
		// the fingerprint still moves when the plugin appears or disappears.
		return p.Name
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stripAgentFrontmatter removes strippedAgentFrontmatter from an agent
// definition's YAML frontmatter. Content with no frontmatter, or none of
// those fields, is returned byte-for-byte; only a file that actually carried
// one is re-serialized. Frontmatter that does not parse is an error rather
// than a pass-through: a block this function cannot read is a block it cannot
// vouch for.
func stripAgentFrontmatter(content string) (string, error) {
	const fence = "---"
	text := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(text, fence+"\n") {
		return content, nil
	}
	rest := text[len(fence)+1:]
	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		return "", fmt.Errorf("frontmatter has no closing %q fence", fence)
	}
	block := rest[:end]
	after := rest[end+1+len(fence):]
	// Drop the remainder of the closing fence line, keeping the body intact.
	if nl := strings.Index(after, "\n"); nl >= 0 {
		after = after[nl+1:]
	} else {
		after = ""
	}

	var fm map[string]any
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return "", fmt.Errorf("frontmatter yaml: %w", err)
	}
	removed := false
	for _, k := range strippedAgentFrontmatter {
		if _, ok := fm[k]; ok {
			delete(fm, k)
			removed = true
		}
	}
	if !removed {
		return content, nil
	}
	out, err := yaml.Marshal(fm)
	if err != nil {
		return "", fmt.Errorf("re-marshal frontmatter: %w", err)
	}
	return fence + "\n" + string(out) + fence + "\n" + after, nil
}
