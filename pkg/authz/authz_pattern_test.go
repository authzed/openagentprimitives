package authz

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The kubebuilder markers on PermissionCheck are the admission-layer half of
// permsurface's charset validation. If they are dropped, a malformed
// permission reaches the cluster and is only caught at runtime, so this test
// guards the marker text itself.
//
// Note: controller-gen is invoked with paths=./pkg/apis/... and
// paths=./pkg/controllers/..., NOT pkg/authz — but it follows type references
// transitively, so these markers DO reach the generated CRDs (verified: 16
// pattern constraints across 4 CRDs). This test plus
// TestInstallYAMLMatchesKustomize is what keeps that true.
func TestPermissionCheck_carriesKubebuilderPatterns(t *testing.T) {
	src, err := os.ReadFile("authz.go")
	require.NoError(t, err)
	text := string(src)

	idx := strings.Index(text, "type PermissionCheck struct")
	require.NotEqual(t, -1, idx, "PermissionCheck must exist in authz.go")
	block := text[idx:]
	if end := strings.Index(block, "\n}"); end != -1 {
		block = block[:end]
	}

	assert.Contains(t, block, "+kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*$`",
		"ResourceType must carry the SpiceDB object-type pattern")
	assert.Contains(t, block, "+kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`",
		"Permission must carry the SpiceDB permission-name pattern")
}
