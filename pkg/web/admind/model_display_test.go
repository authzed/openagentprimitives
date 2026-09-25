package admind

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestModelDisplay is an internal (package admind) test because modelDisplay
// is unexported — it mirrors the runner's servedModelDisplay composition
// rule (pkg/agent/runner/loop.go): "<provider>/<name>" when both are set, a
// bare name when provider is empty, empty stays empty.
func TestModelDisplay(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		{
			name:     "provider and name both set -> prefixed",
			provider: "anthropic",
			model:    "claude-opus-4-8",
			want:     "anthropic/claude-opus-4-8",
		},
		{
			name:     "empty provider -> bare name, no slash",
			provider: "",
			model:    "claude-opus-4-8",
			want:     "claude-opus-4-8",
		},
		{
			name:     "both empty -> empty",
			provider: "",
			model:    "",
			want:     "",
		},
		{
			name:     "openrouter routed model already carries its own provider segment -> double-prefixed verbatim",
			provider: "openrouter",
			model:    "anthropic/claude-3.5-sonnet",
			want:     "openrouter/anthropic/claude-3.5-sonnet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, modelDisplay(tc.provider, tc.model))
		})
	}
}

// TestBareModel is an internal (package admind) test because bareModel is
// unexported — it is modelDisplay's inverse for price lookups: strip the
// FIRST "/"-segment (the AP provider prefix) so PriceMap.Estimate's exact
// map-key lookup hits the bare-keyed price tables/catalog overrides.
func TestBareModel(t *testing.T) {
	cases := []struct {
		name    string
		display string
		want    string
	}{
		{
			name:    "provider-prefixed direct model -> strips provider",
			display: "anthropic/claude-opus-4-8",
			want:    "claude-opus-4-8",
		},
		{
			name:    "openrouter routed model -> only the FIRST segment strips",
			display: "openrouter/anthropic/claude-3.5-sonnet",
			want:    "anthropic/claude-3.5-sonnet",
		},
		{
			name:    "openrouter auto with its own provider-shaped model id -> only the FIRST segment strips",
			display: "openrouter/openrouter/auto",
			want:    "openrouter/auto",
		},
		{
			name:    "no slash -> unchanged",
			display: "claude-opus-4-8",
			want:    "claude-opus-4-8",
		},
		{
			name:    "empty -> empty",
			display: "",
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bareModel(tc.display))
		})
	}
}
