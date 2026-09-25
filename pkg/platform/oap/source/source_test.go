package source_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/oap/source"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

func TestFolderSource_Bundle(t *testing.T) {
	ctx := context.Background()
	b, err := source.OpenFolder(oaptest.WriteBundle(t)).Bundle(ctx)
	require.NoError(t, err)
	require.NotNil(t, b.Manifest)
	// agent.name is derived from the bundled AgentClass (metadata.name).
	assert.Equal(t, "demo-class", b.Manifest.Agent.Name)
	assert.NotEmpty(t, b.Manifests)
}
