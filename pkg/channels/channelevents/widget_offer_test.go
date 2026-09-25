package channelevents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestWidgetOfferPayload_RoundTrip(t *testing.T) {
	in := channelevents.WidgetOfferPayload{
		ArtifactID:   "artifact-abc123",
		Tool:         "mcpserver/widgets.render_form",
		RendererKind: "mcpui",
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"artifactId":"artifact-abc123"`)
	assert.Contains(t, string(b), `"tool":"mcpserver/widgets.render_form"`)
	assert.Contains(t, string(b), `"rendererKind":"mcpui"`)

	var out channelevents.WidgetOfferPayload
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}
