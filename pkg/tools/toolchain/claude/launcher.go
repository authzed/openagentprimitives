// Package claude holds launcher-side logic specific to the "claude" toolchain
// (Claude Code, the inner coding agent running inside the sandbox). It is
// deliberately separate from pkg/tools/toolchain/kinds and
// pkg/tools/toolchain/resolve: those packages know how to deliver a
// toolchain's payload into the pod, not how any one payload's own binary
// discovers its configuration once it is running. The layout of ~/.claude is
// Claude Code knowledge, not podspec knowledge — putting it in the generic
// pod builder would make a future codex or gemini toolchain a branch there.
package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/skills/pluginlayout"
)

// pluginsSubdir is where BridgeStagedSkills rebuilds plugins, under $HOME. It
// is AP's own directory, not Claude Code's: a plugin is loaded from here only
// because the shim names it with --plugin-dir, never by discovery.
const pluginsSubdir = ".oap/claude-plugins"

// BridgeStagedSkills makes AP-staged skill bundles visible to Claude Code,
// and returns the plugin directories the caller must pass to it as
// --plugin-dir, in a deterministic order.
//
// AP stages bundles read-only at /skills/<name>/ (see
// pkg/platform/podspec.applyMounts). A standalone skill is symlinked into
// $HOME/.claude/skills/, where Claude Code discovers user-level skills.
//
// A skill staged with plugin context (pkg/tools/skills/pluginlayout) is
// instead loaded as part of a plugin rebuilt under $HOME: an AP-written
// manifest, the skill directory symlinked in at skills/<name>, and the
// plugin's agents COPIED to agents/. That registers the agents under the
// "<plugin>:<agent>" names the skill's own instructions dispatch, and lets
// paths the skill writes relative to its plugin root resolve. The agents are
// copied rather than linked because Claude Code does not register a plugin
// agent whose file is a symlink; they are small, so the copy is cheap. Such a
// skill is NOT also linked into ~/.claude/skills, which would load it twice.
//
// Staged skills that name the same plugin share one rebuilt plugin only when
// they carry identical agents — the normal case, several skills of one
// plugin from one source at one ref. Nothing namespaces a plugin's name, so
// two skills from unrelated sources can both call theirs "review"; merging
// them would let whichever sorted first decide what "review:bug-hunter" means
// for the other. When the agents differ, every skill naming that plugin is
// linked standalone instead and the conflict is reported: a degraded skill,
// never one silently running someone else's agent.
//
// The runner issues a turn's tool calls in parallel, so several invocations
// of this function can overlap in one home. Each rebuilt plugin therefore
// lives in a directory named for a digest of its contents, built in a
// private temp directory and renamed into place: an invocation either finds
// the finished directory or creates it, and never deletes or rewrites one
// another invocation's claude may be loading. A plugin no longer staged
// leaves its directory behind, unused — no later invocation names it with
// --plugin-dir — rather than being removed out from under a reader.
//
// /skills itself MUST stay mounted read-only in the sandbox container — this
// function only ever reads from it and writes into $HOME, never back into
// skillsDir.
//
// Absent source dir is not an error: a session with no opted-in skills has no
// /skills mount at all, and the launcher must still exec claude rather than
// failing the tool call. A skill whose plugin context cannot be assembled is
// linked standalone instead and named in the returned error, alongside any
// plugin directories that did assemble — a degraded skill, never a lost one.
func BridgeStagedSkills(skillsDir, home string) ([]string, error) {
	entries, err := os.ReadDir(skillsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read staged skills at %s: %w", skillsDir, err)
	}
	userSkills := filepath.Join(home, ".claude", "skills")
	pluginsRoot := filepath.Join(home, pluginsSubdir)
	for _, d := range []string{userSkills, pluginsRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}

	var (
		standalone []string
		groups     = map[string]*pluginGroup{}
		conflicted = map[string]bool{}
		errs       []error
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(skillsDir, e.Name())
		name, ok, err := readPluginName(skillDir)
		var agents map[string][]byte
		if err == nil && ok {
			agents, err = readStagedAgents(skillDir)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("skill %s: plugin context unusable, linking it standalone: %w", e.Name(), err))
		}
		if err != nil || !ok {
			standalone = append(standalone, e.Name())
			continue
		}
		g := groups[name]
		if g == nil {
			g = &pluginGroup{name: name, agents: agents}
			groups[name] = g
		} else if !maps.EqualFunc(g.agents, agents, bytes.Equal) {
			conflicted[name] = true
		}
		g.skills = append(g.skills, e.Name())
	}

	var pluginDirs []string
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		g := groups[name]
		if conflicted[name] {
			errs = append(errs, fmt.Errorf(
				"skills %s each stage a plugin named %s with different agents; linking them all standalone so none dispatches another's agents",
				strings.Join(g.skills, ", "), name))
			standalone = append(standalone, g.skills...)
			continue
		}
		pdir, err := materializePlugin(pluginsRoot, skillsDir, g)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %s (skills %s): plugin context unusable, linking it standalone: %w",
				name, strings.Join(g.skills, ", "), err))
			standalone = append(standalone, g.skills...)
			continue
		}
		for _, sk := range g.skills {
			// Drop a standalone link an earlier invocation may have made, so
			// the skill is not loaded a second time outside its plugin.
			link := filepath.Join(userSkills, sk)
			if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove standalone link %s: %w", link, err))
			}
		}
		pluginDirs = append(pluginDirs, pdir)
	}

	for _, sk := range standalone {
		if err := linkSkill(filepath.Join(skillsDir, sk), filepath.Join(userSkills, sk), pluginsRoot); err != nil {
			return pluginDirs, errors.Join(append(errs, err)...)
		}
	}
	return pluginDirs, errors.Join(errs...)
}

// pluginGroup is the staged skills that share one rebuilt plugin.
type pluginGroup struct {
	name   string
	agents map[string][]byte // agent file name → content
	skills []string          // staged skill directory names, in ReadDir order
}

// readPluginName reports the plugin a staged skill belongs to. ok is false
// for a standalone skill (no manifest staged).
func readPluginName(skillDir string) (name string, ok bool, err error) {
	raw, err := os.ReadFile(filepath.Join(skillDir, filepath.FromSlash(pluginlayout.ManifestPath())))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var m pluginlayout.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false, fmt.Errorf("parse plugin manifest: %w", err)
	}
	if !pluginlayout.ValidName(m.Name) {
		return "", false, fmt.Errorf("plugin manifest name %q is invalid", m.Name)
	}
	return m.Name, true, nil
}

// readStagedAgents reads a staged skill's plugin agents, keyed by file name.
// Anything that is not a validly named *.md file is not an agent and is
// ignored.
func readStagedAgents(skillDir string) (map[string][]byte, error) {
	agentsSrc := filepath.Join(skillDir, pluginlayout.Dir, pluginlayout.AgentsDir)
	entries, err := os.ReadDir(agentsSrc)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read staged agents: %w", err)
	}
	agents := map[string][]byte{}
	for _, a := range entries {
		stem, isMD := strings.CutSuffix(a.Name(), pluginlayout.AgentExt)
		if a.IsDir() || !isMD || !pluginlayout.ValidAgentName(stem) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(agentsSrc, a.Name()))
		if err != nil {
			return nil, fmt.Errorf("read staged agent %s: %w", a.Name(), err)
		}
		agents[a.Name()] = body
	}
	return agents, nil
}

// materializePlugin returns the directory holding g's rebuilt plugin, under
// pluginsRoot, building it if no invocation has yet. The directory is named
// for a digest of everything written into it, so an existing one is already
// exactly what this call would build; a new one is assembled in a private
// temp directory and renamed into place, so no reader ever sees it partial.
func materializePlugin(pluginsRoot, skillsDir string, g *pluginGroup) (string, error) {
	final := filepath.Join(pluginsRoot, g.name+"-"+g.digest(skillsDir))
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}
	tmp, err := os.MkdirTemp(pluginsRoot, ".build-"+g.name+"-")
	if err != nil {
		return "", fmt.Errorf("create plugin build dir: %w", err)
	}
	if err := writePlugin(tmp, skillsDir, g); err != nil {
		return "", errors.Join(err, removeBuildDir(tmp))
	}
	if err := os.Rename(tmp, final); err != nil {
		// A concurrent invocation renamed the same contents into place first.
		if _, statErr := os.Stat(final); statErr == nil {
			return final, removeBuildDir(tmp)
		}
		return "", errors.Join(fmt.Errorf("move plugin into place: %w", err), removeBuildDir(tmp))
	}
	return final, nil
}

// removeBuildDir discards an abandoned plugin build directory.
func removeBuildDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove plugin build dir %s: %w", dir, err)
	}
	return nil
}

// writePlugin lays g out as a Claude Code plugin rooted at pdir.
func writePlugin(pdir, skillsDir string, g *pluginGroup) error {
	for _, d := range []string{".claude-plugin", "skills", "agents"} {
		if err := os.MkdirAll(filepath.Join(pdir, d), 0o755); err != nil {
			return fmt.Errorf("create plugin dir: %w", err)
		}
	}
	// Re-marshal rather than copy the staged manifest, so only the validated
	// name ever reaches Claude Code.
	manifest, err := json.Marshal(pluginlayout.Manifest{Name: g.name})
	if err != nil {
		return fmt.Errorf("marshal plugin manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pdir, filepath.FromSlash(pluginlayout.UpstreamManifestPath)), manifest, 0o644); err != nil {
		return fmt.Errorf("write plugin manifest: %w", err)
	}
	for name, body := range g.agents {
		if err := os.WriteFile(filepath.Join(pdir, "agents", name), body, 0o644); err != nil {
			return fmt.Errorf("copy staged agent %s: %w", name, err)
		}
	}
	for _, sk := range g.skills {
		if err := os.Symlink(filepath.Join(skillsDir, sk), filepath.Join(pdir, "skills", sk)); err != nil {
			return fmt.Errorf("link skill %s into plugin: %w", sk, err)
		}
	}
	return nil
}

// digest identifies the plugin writePlugin would build for g: its name, the
// skills it links and where they point, and its agents.
func (g *pluginGroup) digest(skillsDir string) string {
	h := sha256.New()
	field := func(s string) { fmt.Fprintf(h, "%d:%s|", len(s), s) }
	field(g.name)
	field(skillsDir)
	for _, sk := range slices.Sorted(slices.Values(g.skills)) {
		field("skill")
		field(sk)
	}
	for _, name := range slices.Sorted(maps.Keys(g.agents)) {
		field("agent")
		field(name)
		field(string(g.agents[name]))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// linkSkill (re)points link at target. Idempotent: the launcher runs on every
// claude invocation. The new link is made under scratchDir and renamed over
// the old one, so a concurrent invocation never finds the link missing, and
// never collides with this one recreating it. scratchDir must be on the same
// filesystem as link and outside any directory Claude Code discovers from.
func linkSkill(target, link, scratchDir string) error {
	tmp := filepath.Join(scratchDir, fmt.Sprintf(".link-%d-%d", os.Getpid(), rand.Uint64()))
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("link skill %s: %w", filepath.Base(target), err)
	}
	if err := os.Rename(tmp, link); err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return fmt.Errorf("link skill %s: %w", filepath.Base(target), err)
	}
	return nil
}
