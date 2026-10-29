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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// /skills itself MUST stay mounted read-only in the sandbox container — this
// function only ever reads from it and writes into $HOME, never back into
// skillsDir. The plugin directory is rebuilt from scratch on every call, so a
// skill that is no longer staged leaves nothing behind.
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
	if err := os.MkdirAll(userSkills, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", userSkills, err)
	}
	pluginsRoot := filepath.Join(home, pluginsSubdir)
	if err := os.RemoveAll(pluginsRoot); err != nil {
		return nil, fmt.Errorf("clear stale plugins at %s: %w", pluginsRoot, err)
	}

	var (
		pluginDirs []string
		assembled  = map[string]bool{}
		errs       []error
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(skillsDir, e.Name())
		link := filepath.Join(userSkills, e.Name())

		name, ok, err := readPluginName(skillDir)
		if err == nil && ok {
			var pdir string
			var warn error
			pdir, warn, err = assemblePlugin(pluginsRoot, name, e.Name(), skillDir)
			if warn != nil {
				errs = append(errs, fmt.Errorf("skill %s: %w", e.Name(), warn))
			}
			if err == nil {
				// Drop a standalone link an earlier invocation may have made,
				// so the skill is not loaded a second time outside its plugin.
				if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, fmt.Errorf("remove standalone link %s: %w", link, err))
				}
				if !assembled[pdir] {
					assembled[pdir] = true
					pluginDirs = append(pluginDirs, pdir)
				}
				continue
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("skill %s: plugin context unusable, linking it standalone: %w", e.Name(), err))
		}
		if err := linkSkill(skillDir, link); err != nil {
			return pluginDirs, errors.Join(append(errs, err)...)
		}
	}
	return pluginDirs, errors.Join(errs...)
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

// assemblePlugin adds one staged skill to the plugin rebuilt at
// pluginsRoot/<name>, creating the plugin on first use, and returns the
// plugin's directory. Two staged skills from the same plugin share it. warn
// reports agents left out of an otherwise usable plugin; err means the skill
// is not in the plugin at all and must be linked standalone.
func assemblePlugin(pluginsRoot, name, skillName, skillDir string) (pdir string, warn, err error) {
	pdir = filepath.Join(pluginsRoot, name)
	for _, d := range []string{".claude-plugin", "skills", "agents"} {
		if err := os.MkdirAll(filepath.Join(pdir, d), 0o755); err != nil {
			return "", nil, fmt.Errorf("create plugin dir: %w", err)
		}
	}
	// Re-marshal rather than copy the staged manifest, so only the validated
	// name ever reaches Claude Code.
	manifest, err := json.Marshal(pluginlayout.Manifest{Name: name})
	if err != nil {
		return "", nil, fmt.Errorf("marshal plugin manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pdir, filepath.FromSlash(pluginlayout.UpstreamManifestPath)), manifest, 0o644); err != nil {
		return "", nil, fmt.Errorf("write plugin manifest: %w", err)
	}

	// Copy the agents BEFORE linking the skill in: a failure here then leaves
	// the skill out of the plugin entirely, so the standalone fallback cannot
	// load it a second time.
	agentsSrc := filepath.Join(skillDir, pluginlayout.Dir, pluginlayout.AgentsDir)
	agents, err := os.ReadDir(agentsSrc)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("read staged agents: %w", err)
	}
	var warns []error
	for _, a := range agents {
		stem, isMD := strings.CutSuffix(a.Name(), pluginlayout.AgentExt)
		if a.IsDir() || !isMD || !pluginlayout.ValidAgentName(stem) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(agentsSrc, a.Name()))
		if err != nil {
			return "", nil, fmt.Errorf("read staged agent %s: %w", a.Name(), err)
		}
		dest := filepath.Join(pdir, "agents", a.Name())
		if existing, err := os.ReadFile(dest); err == nil {
			// Another staged skill of the same plugin already provided this
			// agent. Identical is the normal case (same source, same ref);
			// different means two refs of one plugin, and the first wins.
			if !bytes.Equal(existing, body) {
				warns = append(warns, fmt.Errorf(
					"agent %s differs from the copy another skill of plugin %s staged; keeping that one", a.Name(), name))
			}
			continue
		}
		if err := os.WriteFile(dest, body, 0o644); err != nil {
			return "", nil, fmt.Errorf("copy staged agent %s: %w", a.Name(), err)
		}
	}
	if err := linkSkill(skillDir, filepath.Join(pdir, "skills", skillName)); err != nil {
		return "", nil, err
	}
	return pdir, errors.Join(warns...), nil
}

// linkSkill (re)points link at target. Idempotent: the launcher runs on every
// claude invocation.
func linkSkill(target, link string) error {
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace stale link %s: %w", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("link skill %s: %w", filepath.Base(target), err)
	}
	return nil
}
