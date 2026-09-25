package v1alpha1_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestShippedClaudeToolchainValidates parses the SpiceboxToolchain CR the install
// bundle ships and runs it through the same validation the admission path
// enforces. A bad env template, or a prefix that is not <root>/<name>, would
// otherwise only surface at apply time on a user's cluster.
func TestShippedClaudeToolchainValidates(t *testing.T) {
	b, err := os.ReadFile("../../../config/toolchains/claude.yaml")
	require.NoError(t, err)

	var tc spiceboxv1alpha1.SpiceboxToolchain
	require.NoError(t, yaml.Unmarshal(b, &tc))

	assert.Equal(t, "claude", tc.Name)
	assert.Equal(t, spiceboxv1alpha1.ToolchainRootPath+"/claude", tc.Spec.Source.Prefix,
		"prefix must be <ToolchainRootPath>/<name>")
	assert.Positive(t, tc.Spec.SizeBytes, "sizeBytes must be a positive on-disk measurement")
	assert.NoError(t, spiceboxv1alpha1.ValidateToolchainSpec(tc.Name, tc.Spec))
}
