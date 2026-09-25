package meta

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSkill(t *testing.T) {
	tool := NewLoadSkill(map[string]string{
		"github.com/o/r//skills/x@v1": "Full instructions for X.",
	})

	// The body is fetched from whatever git repo/ref a SkillSource names
	// (skillfetch.Request; an empty Ref means the default BRANCH), so anyone who
	// can push to that ref authors it. Result.Trusted means the content is
	// framework-CONTROLLED, not that a framework tool returned it — and it
	// short-circuits Loop.inspectUntrustedResult, which is the only content
	// inspection a meta tool ever gets. A skill body is by design instructions
	// the model is told to follow, so it is a higher-value injection sink than
	// read_channel_history, which IS inspected.
	t.Run("known skill: body returned NOT Trusted, so content guards inspect it", func(t *testing.T) {
		res, err := tool.Execute(context.Background(), []byte(`{"skill":"github.com/o/r//skills/x@v1"}`), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Equal(t, "Full instructions for X.", res.Content)
		assert.False(t, res.Trusted,
			"a third-party SKILL.md body is not framework-controlled and must not skip content-guard inspection")
	})

	t.Run("unknown skill: error listing available names, Trusted (platform-authored)", func(t *testing.T) {
		res, err := tool.Execute(context.Background(), []byte(`{"skill":"nope//y"}`), nil)
		require.NoError(t, err)
		assert.True(t, res.IsError)
		assert.Contains(t, res.Content, "github.com/o/r//skills/x@v1")
		assert.True(t, res.Trusted,
			"the not-found text is written by this package; withholding it would strand the model")
	})

	t.Run("metadata", func(t *testing.T) {
		assert.Equal(t, "load_skill", tool.Name())
	})
}
