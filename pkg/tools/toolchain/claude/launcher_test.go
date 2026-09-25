package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLauncher_LinksStagedSkillsIntoClaudeDiscoveryPath(t *testing.T) {
	home := t.TempDir()
	skills := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(skills, "code-review"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skills, "code-review", "SKILL.md"), []byte("# demo"), 0o644))

	require.NoError(t, LinkStagedSkills(skills, home))

	linked := filepath.Join(home, ".claude", "skills", "code-review", "SKILL.md")
	body, err := os.ReadFile(linked)
	require.NoError(t, err, "Claude Code must find the skill at $HOME/.claude/skills/<name>/")
	assert.Equal(t, "# demo", string(body))
}

func TestLinkStagedSkills_AbsentSkillsDirIsNotAnError(t *testing.T) {
	// A session with no opted-in skills has no /skills mount at all. The
	// launcher must still exec claude rather than failing the tool call.
	require.NoError(t, LinkStagedSkills(filepath.Join(t.TempDir(), "nope"), t.TempDir()))
}

func TestLinkStagedSkills_IsIdempotent(t *testing.T) {
	// The launcher runs on EVERY claude invocation, and a session makes many.
	// A second run must not fail on an existing symlink.
	home, skills := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(skills, "code-review"), 0o755))
	require.NoError(t, LinkStagedSkills(skills, home))
	require.NoError(t, LinkStagedSkills(skills, home), "second run must be a no-op, not a failure")
}
