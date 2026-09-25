package oaptest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// TestWriteBundle_LoadsAsValidBundle pins the fixture's neutral names so the
// tests that assert against it can't drift. agent.name is derived from the
// bundled AgentClass, so the agent takes its class's name, "demo-class".
func TestWriteBundle_LoadsAsValidBundle(t *testing.T) {
	dir := oaptest.WriteBundle(t)

	b, err := oap.FromFolder(dir)
	require.NoError(t, err)
	require.NoError(t, b.Validate())
	assert.Equal(t, "demo-class", b.Manifest.Agent.Name)
	assert.Equal(t, "1.2.0", b.Manifest.Agent.Version)

	crs, err := b.CRs()
	require.NoError(t, err)
	require.Len(t, crs, 1)
	assert.Equal(t, "AgentClass", crs[0].GetKind())
	assert.Equal(t, "demo-class", crs[0].GetName())
	require.Contains(t, b.Assets, "assets/logo.txt")
}
