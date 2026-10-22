package capability

import (
	"context"
	"testing"

	ap "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	htmlrenderer "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Input-only report channels need production tools even with no outbound asset
// capability. The trigger conclusion publishes the artifact's authenticated URL.
func TestInputOnlyBindingOffersHTMLArtifactPreparation(t *testing.T) {
	assetregistry.Reset()
	t.Cleanup(assetregistry.Reset)
	assetregistry.Register(&htmlrenderer.Renderer{})
	capability, ok := Lookup("artifacts")
	require.True(t, ok)
	config, err := capability.ParseConfig([]byte(`{"renderers":["html"]}`))
	require.NoError(t, err)
	tools, skip := capability.Offer(OfferContext{
		Ctx: context.Background(), Granted: true, Enabled: true, Config: config,
		Binding: &ap.ChannelBinding{Kind: "synthetic-trigger", Name: "reports-input", Capabilities: nil},
		Env:     RunnerEnv{Artifacts: fakeArtifactService()},
	})
	require.Nil(t, skip)
	found := false
	for _, tool := range tools {
		if tool.Name() == "artifact_prepare" {
			found = true
			assert.Contains(t, tool.Description(), "html")
		}
	}
	assert.True(t, found, "input-only binding must expose artifact_prepare without a dummy output")
}
