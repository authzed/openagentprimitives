package spiceboxclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

// TestSensitiveEnvNames asserts the reserved set is derived from the
// builtin toolkits' sensitive env vars — exact names, no prefixes. The
// gh toolkit declares GITHUB_TOKEN sensitive and GH_HOST not; only the
// former must appear in the reserved set.
func TestSensitiveEnvNames(t *testing.T) {
	t.Run("nil registry: nil set", func(t *testing.T) {
		assert.Nil(t, sensitiveEnvNames(nil))
	})

	t.Run("builtins: contains sensitive credential names, excludes non-sensitive", func(t *testing.T) {
		reg, err := registry.NewWithBuiltins(nil)
		require.NoError(t, err, "registry.NewWithBuiltins")

		got := sensitiveEnvNames(reg)
		assert.Contains(t, got, "GITHUB_TOKEN", "gh toolkit declares GITHUB_TOKEN sensitive")
		assert.Contains(t, got, "ANTHROPIC_API_KEY", "claude toolkit declares ANTHROPIC_API_KEY sensitive")
		assert.NotContains(t, got, "GH_HOST", "GH_HOST is not declared sensitive")
	})
}
