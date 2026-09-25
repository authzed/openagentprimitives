package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWorkspaceConfig_SharedClaimName(t *testing.T) {
	w := WorkspaceConfig{Mode: WorkspaceShared, SharedClaimName: "as-1-workspace"}
	assert.Equal(t, "as-1-workspace", w.SharedClaimName)
}

func TestSpiceboxSessionSpec_DefaultEnv(t *testing.T) {
	s := SpiceboxSessionSpec{DefaultEnv: map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null"}}
	assert.Equal(t, "/dev/null", s.DefaultEnv["GIT_CONFIG_GLOBAL"])
}
