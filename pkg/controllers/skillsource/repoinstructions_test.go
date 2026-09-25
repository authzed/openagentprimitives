package skillsource

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRepoInstructions(t *testing.T) {
	t.Run("none discovered: nil, no warning", func(t *testing.T) {
		got, warn := BuildRepoInstructions(DiscoveredRepoInstructions{})
		assert.Nil(t, got)
		assert.Empty(t, warn)
	})

	t.Run("under cap: stored verbatim, not truncated", func(t *testing.T) {
		got, warn := BuildRepoInstructions(DiscoveredRepoInstructions{SourceFile: "AGENTS.md", Content: "short"})
		require.NotNil(t, got)
		assert.Equal(t, "AGENTS.md", got.SourceFile)
		assert.Equal(t, "short", got.Content)
		assert.False(t, got.Truncated)
		assert.Empty(t, warn)
	})

	t.Run("over cap: truncated to cap with marker + warning", func(t *testing.T) {
		big := strings.Repeat("a", maxRepoInstructionsBytes+500)
		got, warn := BuildRepoInstructions(DiscoveredRepoInstructions{SourceFile: "CLAUDE.md", Content: big})
		require.NotNil(t, got)
		assert.True(t, got.Truncated)
		assert.LessOrEqual(t, len(got.Content), maxRepoInstructionsBytes+64, "content bounded to cap plus the short marker")
		assert.Contains(t, got.Content, "truncated")
		assert.Contains(t, warn, "CLAUDE.md")
		assert.Contains(t, warn, "64 KiB")
	})
}
