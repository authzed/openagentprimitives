package workspacesource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git" // register "git"
)

func TestDriverIsResolvableForTranslatedSpec(t *testing.T) {
	d := SpecToDriver(demoWS().Spec)
	drv, ok := registry.Get(d.Kind)
	require.True(t, ok, "git driver must be registered for the translated kind")
	assert.NoError(t, drv.Validate(d), "translated demo spec must validate")
}
