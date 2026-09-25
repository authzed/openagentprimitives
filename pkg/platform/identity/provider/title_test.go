package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderTitlesPopulated(t *testing.T) {
	cases := map[string]string{
		"github-pat":      "GitHub",
		"anthropic-oauth": "Claude",
	}
	for id, wantTitle := range cases {
		t.Run(id+": Title="+wantTitle, func(t *testing.T) {
			p, ok := ByID(id)
			require.True(t, ok, "provider %q must exist", id)
			assert.Equal(t, wantTitle, p.Title)
			assert.NotEmpty(t, p.Description, "description must be user-facing, not empty")
		})
	}
}
