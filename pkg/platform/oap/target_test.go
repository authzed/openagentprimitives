package oap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTarget_AppendMarker(t *testing.T) {
	// `field[]` (empty brackets) marks the terminal segment as append/union:
	// the answer list is merged onto whatever the CR already declares, rather
	// than replacing it. `field[selector]` (non-empty) stays element-selection.
	got, err := ParseTarget("SpiceboxClass/codelike-bundle#spec.toolchains[]")
	require.NoError(t, err)
	assert.Equal(t, []Segment{{Field: "spec"}, {Field: "toolchains", Append: true}}, got.Path)
}

func TestParseTarget(t *testing.T) {
	t.Run("scalar path", func(t *testing.T) {
		got, err := ParseTarget("MCPServer/pm-linear#spec.upstream.endpoint")
		require.NoError(t, err)
		assert.Equal(t, "MCPServer", got.Kind)
		assert.Equal(t, "pm-linear", got.Name)
		assert.Equal(t, []Segment{{Field: "spec"}, {Field: "upstream"}, {Field: "endpoint"}}, got.Path)
	})
	t.Run("list selector path", func(t *testing.T) {
		got, err := ParseTarget("AgentClass/pm#spec.boundEntities[github_repo].defaults")
		require.NoError(t, err)
		assert.Equal(t, []Segment{
			{Field: "spec"},
			{Field: "boundEntities", Selector: "github_repo"},
			{Field: "defaults"},
		}, got.Path)
	})

	bad := []struct{ name, in, want string }{
		{"no hash", "AgentClass/pm", "want Kind/name#path"},
		{"no slash", "AgentClass#spec.x", "want Kind/name#path"},
		{"empty kind", "/pm#spec.x", "empty kind or name"},
		{"empty name", "AgentClass/#spec.x", "empty kind or name"},
		{"empty segment", "AgentClass/pm#spec..x", "empty path segment"},
		{"unterminated selector", "AgentClass/pm#spec.boundEntities[github_repo.defaults", "unterminated selector"},
		{"append marker not final", "AgentClass/pm#spec.boundEntities[].defaults", "final segment"},
	}
	for _, tc := range bad {
		t.Run(tc.name+": error", func(t *testing.T) {
			_, err := ParseTarget(tc.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
