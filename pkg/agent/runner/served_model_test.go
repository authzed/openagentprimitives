package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestServedModelDisplay covers Task 7's uniform "<provider>/<served-model>"
// transcript display string: openrouter (which reports a per-turn served
// model that can differ under auto-routing/fallback), anthropic (which
// leaves Response.Model empty, so the configured model is the fallback), and
// the no-provider edge case used by code paths that don't know the provider.
func TestServedModelDisplay(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		respModel  string
		configured string
		want       string
	}{
		{
			name:       "openrouter reports the actually-served model",
			provider:   "openrouter",
			respModel:  "anthropic/claude-3.5-sonnet",
			configured: "openrouter/auto",
			want:       "openrouter/anthropic/claude-3.5-sonnet",
		},
		{
			name:       "anthropic reports no Response.Model, falls back to configured",
			provider:   "anthropic",
			respModel:  "",
			configured: "claude-opus-4-8",
			want:       "anthropic/claude-opus-4-8",
		},
		{
			name:       "empty provider yields a bare model with no leading slash",
			provider:   "",
			respModel:  "claude-opus-4-8",
			configured: "claude-opus-4-8",
			want:       "claude-opus-4-8",
		},
		{
			name:       "empty provider and empty respModel falls back to configured",
			provider:   "",
			respModel:  "",
			configured: "claude-opus-4-8",
			want:       "claude-opus-4-8",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, servedModelDisplay(tc.provider, tc.respModel, tc.configured))
		})
	}
}
