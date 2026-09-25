package markup_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/preview/markup"
)

// TestFake_Generate verifies the Fake provider's deterministic behaviour.
func TestFake_Generate(t *testing.T) {
	ctx := context.Background()

	t.Run("returns default markup when Markup field is empty", func(t *testing.T) {
		f := markup.Fake{}
		got, err := f.Generate(ctx, "some instruction")
		require.NoError(t, err)
		assert.Equal(t, `<div class="preview-sample"><h1>Sample</h1><p>Preview markup.</p></div>`, got)
	})

	t.Run("returns custom Markup verbatim, ignoring instruction", func(t *testing.T) {
		custom := `<section class="hero"><h2>Hello</h2></section>`
		f := markup.Fake{Markup: custom}

		got, err := f.Generate(ctx, "irrelevant instruction")
		require.NoError(t, err)
		assert.Equal(t, custom, got)
	})

	t.Run("Name returns fake", func(t *testing.T) {
		assert.Equal(t, "fake", markup.Fake{}.Name())
	})
}

// TestNewAnthropic verifies constructor invariants without making a live API call.
func TestNewAnthropic(t *testing.T) {
	t.Run("panics on empty API key", func(t *testing.T) {
		assert.Panics(t, func() {
			markup.NewAnthropic("")
		})
	})

	t.Run("returns non-nil provider with a non-empty key", func(t *testing.T) {
		p := markup.NewAnthropic("not-a-real-key")
		require.NotNil(t, p)
		assert.Equal(t, "anthropic", p.Name())
	})
}

// TestBuildSystemPrompt verifies the system prompt's security framing without
// exercising live API calls.
func TestBuildSystemPrompt(t *testing.T) {
	prompt := markup.ExportedSystemPrompt()

	t.Run("forbids script tags", func(t *testing.T) {
		assert.True(t, strings.Contains(prompt, "<script>"),
			"system prompt must explicitly mention <script> to forbid it")
	})

	t.Run("frames class names as USER DATA", func(t *testing.T) {
		assert.True(t, strings.Contains(prompt, "USER DATA"),
			"system prompt must label instruction content as USER DATA")
	})

	t.Run("forbids on* event attributes", func(t *testing.T) {
		assert.True(t, strings.Contains(prompt, "on*"),
			"system prompt must forbid on* event attributes")
	})

	t.Run("forbids html/head/body wrappers", func(t *testing.T) {
		assert.True(t, strings.Contains(prompt, "<body>") || strings.Contains(prompt, "<html>"),
			"system prompt must forbid full HTML wrappers")
	})
}

// TestFake_implementsProvider confirms at compile time that Fake satisfies Provider.
func TestFake_implementsProvider(t *testing.T) {
	var _ markup.Provider = markup.Fake{}
}

// TestAnthropicProvider_implementsProvider confirms at compile time that
// *AnthropicProvider satisfies Provider.
func TestAnthropicProvider_implementsProvider(t *testing.T) {
	// NewAnthropic panics on "" so we use a non-empty dummy key.
	p := markup.NewAnthropic("k")
	var _ markup.Provider = p
}
