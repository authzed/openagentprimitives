package oap_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	oapkind "github.com/authzed/openagentprimitives/pkg/channels/channelassets/oap"
	oapfmt "github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// bundleBytes packs the shared fixture bundle (agent demo-agent) into .oap bytes.
func bundleBytes(t *testing.T) []byte {
	t.Helper()
	b, err := oapfmt.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	raw, err := oapfmt.Pack(b)
	require.NoError(t, err)
	return raw
}

func TestRender_ValidBundle_PassesThroughWithBundleMIMEAndExtension(t *testing.T) {
	raw := bundleBytes(t)
	out, err := oapkind.New().Render(context.Background(), channelassets.Input{Payload: raw, Filename: "demo-agent"})
	require.NoError(t, err)
	assert.Equal(t, raw, out.Bytes, "the bundle is the install artifact; nothing may rewrite it")
	assert.Equal(t, oapfmt.ArtifactType, out.MIME)
	assert.Equal(t, "demo-agent"+oapfmt.DefaultExtension, out.Filename, "the extension is forced")
}

func TestRender_FilenameAlreadyHasTheExtension_KeptOnce(t *testing.T) {
	out, err := oapkind.New().Render(context.Background(), channelassets.Input{Payload: bundleBytes(t), Filename: "demo-agent" + oapfmt.DefaultExtension})
	require.NoError(t, err)
	assert.Equal(t, "demo-agent"+oapfmt.DefaultExtension, out.Filename)
}

func TestRender_EmptyFilename_DefaultsToDraft(t *testing.T) {
	out, err := oapkind.New().Render(context.Background(), channelassets.Input{Payload: bundleBytes(t)})
	require.NoError(t, err)
	assert.Equal(t, "draft"+oapfmt.DefaultExtension, out.Filename)
}

func TestRender_NotABundle_IsMalformedPayload(t *testing.T) {
	_, err := oapkind.New().Render(context.Background(), channelassets.Input{Payload: []byte("not a bundle")})
	require.Error(t, err)
	assert.True(t, errors.Is(err, channelassets.ErrMalformedPayload), "the controller maps this to MalformedInput: %v", err)
}

// The contract that keeps this kind off the agent's menu and out of the live
// view: only the workshop draft route ever creates a render of it.
func TestKind_IsOperatorOnlyStandaloneDownload(t *testing.T) {
	r := oapkind.New()
	assert.Equal(t, "oap", r.Kind())
	assert.False(t, r.AgentSelectable(), "an agent must never be offered this kind")
	assert.False(t, r.SupportsLiveView())
	assert.Equal(t, channelassets.DeliveryStandalone, r.Delivery())
	assert.Equal(t, channelassets.ExecutionModeInProcess, r.ExecutionMode())
	assert.Equal(t, []string{oapfmt.ArtifactType}, r.OutputMIMEs())
	assert.Equal(t, int64(10<<20), r.MaxInputSize())
	assert.Empty(t, r.Instructions(), "nothing to tell an agent about a kind it cannot select")
}
