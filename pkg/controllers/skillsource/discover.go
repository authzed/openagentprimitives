// pkg/controllers/skillsource/discover.go
//
// Package skillsource contains the SkillSource controller and its pure helper
// functions. This file implements Discover, the pure function that walks a
// fetched repo tree and converts SKILL.md files into DiscoveredSkill values
// ready for materialization as Skill CRs.
package skillsource

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/pluginlayout"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillmd"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/validate"
)

// DiscoveredRepoInstructions is the repo-root agent-instructions file found in
// the fetched tree (one per repo, independent of skill count). Zero value
// (SourceFile == "") means no file was present.
type DiscoveredRepoInstructions struct {
	SourceFile string // "AGENTS.md" or "CLAUDE.md"
	Content    string
}

// repoInstructionCandidates is the precedence-ordered list of repo-root
// agent-instruction filenames. AGENTS.md (cross-tool standard) is primary;
// CLAUDE.md is the fallback. First one present wins.
var repoInstructionCandidates = []string{"AGENTS.md", "CLAUDE.md"}

// findRepoInstructions returns the first repo-root instruction file present in
// files (exact name, root only — a path containing no "/"), or ("", nil, false).
func findRepoInstructions(files map[string][]byte) (sourceFile string, content []byte, found bool) {
	for _, cand := range repoInstructionCandidates {
		if c, ok := files[cand]; ok {
			return cand, c, true
		}
	}
	return "", nil, false
}

// DiscoveredSkill is one validated skill extracted from a fetched repo tree,
// ready to be cached and materialized as a Skill CR.
type DiscoveredSkill struct {
	// CanonicalName is the fully-formed canonical skill name
	// (e.g. "github.com/org/repo//skills/foo@v1.0").
	CanonicalName string
	// Description is copied from the frontmatter for quick access.
	Description string
	// Body is the Markdown body of SKILL.md (after the frontmatter).
	Body string
	// Frontmatter is the full parsed frontmatter from SKILL.md.
	Frontmatter skillmd.Frontmatter
	// Subpath is the skill directory path relative to the repo root
	// (e.g. "skills/foo").
	Subpath string
	// BundleDigest is the sha256 hex digest of BundleTarGz. Empty for
	// instruction-only skills (no files other than SKILL.md).
	BundleDigest string
	// BundleTarGz is the deterministic gzipped tar of all files under the
	// skill directory except SKILL.md. Nil for instruction-only skills.
	BundleTarGz []byte
	// Plugin is the Claude Code plugin the skill was published inside (see
	// discoverPlugin), or nil for a standalone skill.
	Plugin *v1.SkillPlugin
}

// DiscoverOptions tunes one Discover pass. The zero value is the default.
type DiscoverOptions struct {
	// SkipPlugins materializes every skill as standalone, even one published
	// inside a plugin — the source's spec.disablePluginAgents.
	SkipPlugins bool
}

// Discover walks files (repo-root-relative path → content), finds every
// SKILL.md under subpath (whole tree when subpath is ""), parses and validates
// each one, and returns the discovered skills sorted by CanonicalName plus a
// slice of human-readable problem strings for any skipped/invalid skills.
//
// repoLocator is already normalized (caller passes canonical.Normalize(spec.RepoURL)).
// ref is appended to the canonical name when non-empty; empty → unpinned name.
func Discover(repoLocator, subpath, ref string, files map[string][]byte, opts DiscoverOptions) ([]DiscoveredSkill, DiscoveredRepoInstructions, []string) {
	var discovered []DiscoveredSkill
	var problems []string

	var repoInstr DiscoveredRepoInstructions
	if f, c, ok := findRepoInstructions(files); ok {
		repoInstr = DiscoveredRepoInstructions{SourceFile: f, Content: string(c)}
	}

	// Counted so a subpath that selected none of the tree's SKILL.md files can
	// be reported below with how many it passed over.
	var skillMDTotal, skillMDUnderSubpath int

	for path, content := range files {
		// Step 1: find paths ending in /SKILL.md (or exactly SKILL.md) that are
		// under the requested subpath. Accept both "/" and exact-match prefixes.
		if !isSkillMD(path) {
			continue
		}
		skillMDTotal++
		if !isUnderSubpath(path, subpath) {
			continue
		}
		skillMDUnderSubpath++

		// The skill directory is the path with its trailing "/SKILL.md" removed.
		skillDir := strings.TrimSuffix(path, "SKILL.md")
		skillDir = strings.TrimSuffix(skillDir, "/")
		// For a top-level SKILL.md (path == "SKILL.md") skillDir collapses to "".
		// That would produce an invalid canonical name; treat it like no dir prefix.
		// In practice the valid form is always "<dir>/SKILL.md".

		// Step 2: parse the SKILL.md.
		doc, err := skillmd.Parse(content)
		if err != nil {
			problems = append(problems, fmt.Sprintf("skipping %s: parse error: %v", path, err))
			continue
		}
		fm := doc.Frontmatter
		body := doc.Body

		// Step 3: build the canonical name.
		var canonicalName string
		if skillDir == "" {
			canonicalName = repoLocator + "//" + "."
		} else {
			canonicalName = repoLocator + "//" + skillDir
		}
		if ref != "" {
			canonicalName += "@" + ref
		}

		// Step 4: validate. validate.Skill checks that fm.Name matches
		// LastSegment() of the canonical name, among other rules.
		if errs := validate.Skill(canonicalName, fm.Name, fm.Description, body); len(errs) != 0 {
			problems = append(problems, fmt.Sprintf("skipping %s: %v", canonicalName, errs[0]))
			continue
		}

		// Step 5: collect all files under the skill directory except SKILL.md,
		// keyed relative to the skill dir, to form the bundle.
		relFiles := map[string][]byte{}
		skillDirPrefix := skillDir + "/"
		for fp, fc := range files {
			if fp == path {
				// Exclude the SKILL.md itself from the bundle.
				continue
			}
			if skillDir == "" {
				// Top-level skill: everything except SKILL.md is a bundle file.
				if !strings.HasPrefix(fp, "/") {
					relFiles[fp] = fc
				}
				continue
			}
			if strings.HasPrefix(fp, skillDirPrefix) {
				rel := strings.TrimPrefix(fp, skillDirPrefix)
				if rel == "" {
					continue
				}
				if pluginlayout.IsReserved(rel) {
					// The reserved directory is filled from spec.plugin at
					// staging and nowhere else; a skill shipping its own
					// would otherwise smuggle agents past discoverPlugin's
					// checks.
					problems = append(problems, fmt.Sprintf(
						"%s: dropping %s from the bundle: %s/ is reserved for the skill's plugin context",
						canonicalName, fp, pluginlayout.Dir))
					continue
				}
				relFiles[rel] = fc
			}
		}

		var bundleDigest string
		var bundleTarGz []byte
		if len(relFiles) > 0 {
			// Instruction-only skills (no supporting files) get no bundle.
			bundleTarGz, bundleDigest, err = skillbundle.TarGz(relFiles)
			if err != nil {
				problems = append(problems, fmt.Sprintf("skipping %s: bundle error: %v", canonicalName, err))
				continue
			}
		}

		// Step 6: capture the enclosing plugin, if any.
		var plugin *v1.SkillPlugin
		if !opts.SkipPlugins {
			var pluginProblems []string
			plugin, pluginProblems = discoverPlugin(canonicalName, skillDir, files)
			problems = append(problems, pluginProblems...)
		}

		// Step 7: append the discovered skill.
		discovered = append(discovered, DiscoveredSkill{
			CanonicalName: canonicalName,
			Description:   fm.Description,
			Body:          body,
			Frontmatter:   fm,
			Subpath:       skillDir,
			BundleDigest:  bundleDigest,
			BundleTarGz:   bundleTarGz,
			Plugin:        plugin,
		})
	}

	// A non-empty subpath that selected nothing is the operator's own claim about
	// where the skill lives pointing somewhere the tree does not have it — a
	// directory renamed upstream, a typo at authoring time, a ref that predates
	// the skill. Nothing else in the pass notices: no SKILL.md was parsed, so no
	// SKILL.md was rejected, and "zero skills, zero problems" is also exactly
	// what a repo with no skills at all produces. Reporting it here is what puts
	// the one-segment drift in status.discoveryProblems, where the AgentClass's
	// own SkillMissing message already sends the operator to look.
	//
	// An empty subpath makes no claim about layout, so it gets no problem; the
	// empty outcome is reported by RecordSync in either case.
	if subpath != "" && skillMDUnderSubpath == 0 {
		problems = append(problems, fmt.Sprintf(
			"subpath %q matched no SKILL.md in the fetched tree (%d SKILL.md file(s) found elsewhere in the repo at this ref); check spec.subpath and spec.ref against the repository's layout",
			subpath, skillMDTotal))
	}

	// Deterministic order: sort by CanonicalName so the reconciler always sees
	// skills in the same order regardless of map iteration.
	sort.Slice(discovered, func(i, j int) bool {
		return discovered[i].CanonicalName < discovered[j].CanonicalName
	})
	// problems is accumulated under the same map-range and lands verbatim in
	// status.discoveryProblems, which the reconciler recomputes on every pass.
	// Unsorted, a flipped order would look like a status change and re-enqueue
	// the reconcile — and its git fetch — indefinitely.
	sort.Strings(problems)

	return discovered, repoInstr, problems
}

// Agent-capture ceilings. They keep a plugin with an unusual number or size of
// agents from pushing its Skill object toward etcd's limit; every agent is
// inline on spec.plugin, and one plugin's agents ride on each of its skills.
// maxPluginAgents MUST match the MaxItems marker on SkillPlugin.Agents: one
// agent past it and the apiserver rejects the whole Skill write, which fails
// the source's sync rather than just costing that skill an agent.
const (
	maxPluginAgents     = 64
	maxPluginAgentBytes = 64 << 10
	maxPluginTotalBytes = 256 << 10
)

// discoverPlugin returns the plugin a skill at skillDir was published inside,
// or nil when it is standalone. A skill belongs to a plugin when its directory
// is <root>/skills/<name> and <root>/.claude-plugin/plugin.json exists — the
// layout Claude Code itself loads plugins from. <root> may be the repo root.
//
// It deliberately reads OUTSIDE the source's subpath: the subpath selects the
// skill, and the skill's own instructions name the plugin's agents, so they
// are part of what was selected. Only the manifest's name and the direct
// agents/*.md children are read; see pluginlayout's package doc for why
// nothing else is. Anything malformed is a problem, never a failure: the skill
// still materializes, standalone or with the agents that did pass.
func discoverPlugin(canonicalName, skillDir string, files map[string][]byte) (*v1.SkillPlugin, []string) {
	if skillDir == "" {
		return nil, nil
	}
	parent := path.Dir(skillDir)
	if path.Base(parent) != pluginlayout.UpstreamSkillsDir {
		return nil, nil
	}
	root := path.Dir(parent)
	manifestPath := path.Join(root, pluginlayout.UpstreamManifestPath)
	raw, ok := files[manifestPath]
	if !ok {
		return nil, nil
	}

	var problems []string
	var manifest struct {
		Name   string          `json:"name"`
		Agents json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, []string{fmt.Sprintf("%s: treating as standalone: %s is not valid JSON: %v",
			canonicalName, manifestPath, err)}
	}
	if !pluginlayout.ValidName(manifest.Name) {
		return nil, []string{fmt.Sprintf("%s: treating as standalone: %s name %q is not a kebab-case plugin name",
			canonicalName, manifestPath, manifest.Name)}
	}
	if declaresAgents(manifest.Agents) {
		problems = append(problems, fmt.Sprintf(
			"%s: %s declares custom agent paths, which are not captured; only %s/*.md is",
			canonicalName, manifestPath, path.Join(root, pluginlayout.UpstreamAgentsDir)))
	}

	agentsPrefix := path.Join(root, pluginlayout.UpstreamAgentsDir) + "/"
	var agents []v1.SkillPluginAgent
	for fp, content := range files {
		rel, ok := strings.CutPrefix(fp, agentsPrefix)
		if !ok || strings.Contains(rel, "/") || !strings.HasSuffix(rel, pluginlayout.AgentExt) {
			continue
		}
		name := strings.TrimSuffix(rel, pluginlayout.AgentExt)
		switch {
		case !pluginlayout.ValidAgentName(name):
			problems = append(problems, fmt.Sprintf("%s: skipping plugin agent %s: %q is not a valid agent name",
				canonicalName, fp, name))
		case len(content) > maxPluginAgentBytes:
			problems = append(problems, fmt.Sprintf("%s: skipping plugin agent %s: %d bytes exceeds the %d-byte limit",
				canonicalName, fp, len(content), maxPluginAgentBytes))
		default:
			agents = append(agents, v1.SkillPluginAgent{Name: name, Content: string(content)})
		}
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })

	// Over either ceiling, keep the alphabetical prefix that fits rather than
	// an arbitrary subset, so the captured set is stable across passes.
	if len(agents) > maxPluginAgents {
		problems = append(problems, fmt.Sprintf(
			"%s: capturing %d of %d plugin agents: the rest exceed the %d-agent limit",
			canonicalName, maxPluginAgents, len(agents), maxPluginAgents))
		agents = agents[:maxPluginAgents]
	}
	total := 0
	for i, a := range agents {
		total += len(a.Content)
		if total > maxPluginTotalBytes {
			problems = append(problems, fmt.Sprintf(
				"%s: capturing %d of %d plugin agents: the rest exceed the %d-byte total limit",
				canonicalName, i, len(agents), maxPluginTotalBytes))
			agents = agents[:i]
			break
		}
	}
	return &v1.SkillPlugin{Name: manifest.Name, Agents: agents}, problems
}

// declaresAgents reports whether a plugin.json "agents" value actually names
// something. An absent key, null, an empty string and an empty list all mean
// the default agents/ directory, so none of them is worth a problem.
func declaresAgents(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", `""`, "[]":
		return false
	}
	return true
}

// isSkillMD reports whether path refers to a SKILL.md file (either at repo root
// or inside a directory).
func isSkillMD(path string) bool {
	return path == "SKILL.md" || strings.HasSuffix(path, "/SKILL.md")
}

// isUnderSubpath reports whether path is under the given subpath prefix.
// When subpath is empty the whole tree is included.
func isUnderSubpath(path, subpath string) bool {
	if subpath == "" {
		return true
	}
	// Accept subpath/ prefix (file is inside the subtree).
	return strings.HasPrefix(path, subpath+"/")
}
