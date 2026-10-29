package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/skills/pluginlayout"
)

// stageSkill lays out /skills/<name>/SKILL.md the way the sandbox mount does.
func stageSkill(t *testing.T, skills, name string) string {
	t.Helper()
	dir := filepath.Join(skills, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# "+name), 0o644))
	return dir
}

// stagePlugin adds staged plugin context (pluginlayout) to a staged skill.
func stagePlugin(t *testing.T, skillDir, manifest string, agents map[string]string) {
	t.Helper()
	agentsDir := filepath.Join(skillDir, pluginlayout.Dir, pluginlayout.AgentsDir)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, filepath.FromSlash(pluginlayout.ManifestPath())), []byte(manifest), 0o644))
	for name, body := range agents {
		require.NoError(t, os.WriteFile(filepath.Join(agentsDir, name), []byte(body), 0o644))
	}
}

func TestBridgeStagedSkills_LinksStandaloneSkillIntoClaudeDiscoveryPath(t *testing.T) {
	home, skills := t.TempDir(), t.TempDir()
	stageSkill(t, skills, "code-review")

	dirs, err := BridgeStagedSkills(skills, home)
	require.NoError(t, err)

	assert.Empty(t, dirs, "a standalone skill needs no --plugin-dir")
	body, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "code-review", "SKILL.md"))
	require.NoError(t, err, "Claude Code must find the skill at $HOME/.claude/skills/<name>/")
	assert.Equal(t, "# code-review", string(body))
}

func TestBridgeStagedSkills_AbsentSkillsDirIsNotAnError(t *testing.T) {
	// A session with no opted-in skills has no /skills mount at all. The
	// launcher must still exec claude rather than failing the tool call.
	dirs, err := BridgeStagedSkills(filepath.Join(t.TempDir(), "nope"), t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, dirs)
}

func TestBridgeStagedSkills_RebuildsPluginForSkillWithPluginContext(t *testing.T) {
	home, skills := t.TempDir(), t.TempDir()
	dir := stageSkill(t, skills, "code-review")
	stagePlugin(t, dir, `{"name":"demo-plugin"}`, map[string]string{
		"bug-hunter.md": "---\nname: bug-hunter\n---\nprompt",
		"notes.txt":     "not an agent",
	})

	dirs, err := BridgeStagedSkills(skills, home)
	require.NoError(t, err)

	pdir := filepath.Join(home, pluginsSubdir, "demo-plugin")
	assert.Equal(t, []string{pdir}, dirs)

	manifest, err := os.ReadFile(filepath.Join(pdir, ".claude-plugin", "plugin.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"demo-plugin"}`, string(manifest))

	agent := filepath.Join(pdir, "agents", "bug-hunter.md")
	info, err := os.Lstat(agent)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "agents must be copied: Claude Code does not register a symlinked plugin agent")
	assert.NoFileExists(t, filepath.Join(pdir, "agents", "notes.txt"), "only *.md files are agents")

	body, err := os.ReadFile(filepath.Join(pdir, "skills", "code-review", "SKILL.md"))
	require.NoError(t, err, "the skill must load from inside its plugin")
	assert.Equal(t, "# code-review", string(body))

	_, err = os.Lstat(filepath.Join(home, ".claude", "skills", "code-review"))
	assert.ErrorIs(t, err, os.ErrNotExist, "a plugin skill must not also load standalone")
}

func TestBridgeStagedSkills_TwoSkillsOfOnePluginShareIt(t *testing.T) {
	home, skills := t.TempDir(), t.TempDir()
	for _, name := range []string{"review-a", "review-b"} {
		stagePlugin(t, stageSkill(t, skills, name), `{"name":"demo-plugin"}`,
			map[string]string{"bug-hunter.md": "prompt"})
	}

	dirs, err := BridgeStagedSkills(skills, home)
	require.NoError(t, err)

	require.Len(t, dirs, 1, "one plugin, one --plugin-dir")
	assert.FileExists(t, filepath.Join(dirs[0], "skills", "review-a", "SKILL.md"))
	assert.FileExists(t, filepath.Join(dirs[0], "skills", "review-b", "SKILL.md"))
}

func TestBridgeStagedSkills_ConflictingAgentKeepsFirstAndWarns(t *testing.T) {
	home, skills := t.TempDir(), t.TempDir()
	stagePlugin(t, stageSkill(t, skills, "review-a"), `{"name":"demo-plugin"}`, map[string]string{"bug-hunter.md": "first"})
	stagePlugin(t, stageSkill(t, skills, "review-b"), `{"name":"demo-plugin"}`, map[string]string{"bug-hunter.md": "second"})

	dirs, err := BridgeStagedSkills(skills, home)

	require.Error(t, err, "a conflict must be reported, not swallowed")
	assert.Contains(t, err.Error(), "bug-hunter.md")
	require.Len(t, dirs, 1, "the conflict degrades one agent, not the plugin")
	body, rerr := os.ReadFile(filepath.Join(dirs[0], "agents", "bug-hunter.md"))
	require.NoError(t, rerr)
	assert.Equal(t, "first", string(body))
	assert.FileExists(t, filepath.Join(dirs[0], "skills", "review-b", "SKILL.md"), "the second skill still loads inside the plugin")
	_, lerr := os.Lstat(filepath.Join(home, ".claude", "skills", "review-b"))
	assert.ErrorIs(t, lerr, os.ErrNotExist, "and not a second time standalone")
}

func TestBridgeStagedSkills_UnusableManifestFallsBackToStandalone(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
	}{
		{name: "malformed JSON: linked standalone, reported", manifest: `{`},
		{name: "name that would escape the plugins dir: linked standalone, reported", manifest: `{"name":"../../etc"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, skills := t.TempDir(), t.TempDir()
			stagePlugin(t, stageSkill(t, skills, "code-review"), tc.manifest, map[string]string{"x.md": "prompt"})

			dirs, err := BridgeStagedSkills(skills, home)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "linking it standalone")
			assert.Empty(t, dirs)
			assert.FileExists(t, filepath.Join(home, ".claude", "skills", "code-review", "SKILL.md"),
				"a degraded skill is still a loaded skill")
		})
	}
}

func TestBridgeStagedSkills_IsIdempotentAndDropsUnstagedPlugins(t *testing.T) {
	// The launcher runs on EVERY claude invocation, and a session makes many.
	home, skills := t.TempDir(), t.TempDir()
	stagePlugin(t, stageSkill(t, skills, "code-review"), `{"name":"demo-plugin"}`, map[string]string{"a.md": "prompt"})
	stageSkill(t, skills, "standalone")

	first, err := BridgeStagedSkills(skills, home)
	require.NoError(t, err)
	second, err := BridgeStagedSkills(skills, home)
	require.NoError(t, err, "second run must be a no-op, not a failure")
	assert.Equal(t, first, second)

	require.NoError(t, os.RemoveAll(filepath.Join(skills, "code-review")))
	third, err := BridgeStagedSkills(skills, home)
	require.NoError(t, err)
	assert.Empty(t, third)
	assert.NoDirExists(t, filepath.Join(home, pluginsSubdir, "demo-plugin"),
		"a plugin whose skill is no longer staged must not linger")
}
