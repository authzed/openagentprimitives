package mcpui_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/mcpui"
)

func TestKind(t *testing.T) {
	assert.Equal(t, "mcpui", mcpui.New().Kind())
}

// TestRender_Identity_NoStripping is the core contract of this renderer:
// unlike the html kind (bluemonday sanitize + CSP <meta> injection), mcpui
// must return the payload byte-for-byte, including a <script> tag that the
// html kind would strip.
func TestRender_Identity_NoStripping(t *testing.T) {
	const payload = `<html><head></head><body><script>window.parent.postMessage({type:"ready"},"*")</script><div id="widget">hi</div></body></html>`

	r := mcpui.New()
	out, err := r.Render(context.Background(), channelassets.Input{Payload: []byte(payload), Filename: "widget.html"})
	require.NoError(t, err)

	assert.Equal(t, payload, string(out.Bytes), "mcpui must return input bytes verbatim — no sanitize, no CSP injection")
	assert.Equal(t, "text/html", out.MIME)
	assert.Contains(t, string(out.Bytes), "<script>", "script tag must survive (html kind would strip it)")
	assert.Empty(t, out.Warnings, "identity renderer produces no sanitizer warnings")
}

func TestRender_DefaultsFilename(t *testing.T) {
	r := mcpui.New()
	out, err := r.Render(context.Background(), channelassets.Input{Payload: []byte("<p>x</p>")})
	require.NoError(t, err)
	assert.Equal(t, "widget.html", out.Filename)
}

func TestRender_OversizedInput_Errors(t *testing.T) {
	big := make([]byte, (1<<20)+1)
	r := mcpui.New()
	_, err := r.Render(context.Background(), channelassets.Input{Payload: big})
	require.Error(t, err)
}

func TestRender_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := mcpui.New()
	_, err := r.Render(ctx, channelassets.Input{Payload: []byte("<p>x</p>")})
	require.Error(t, err)
}

func TestServeTransform_NoOp(t *testing.T) {
	r := mcpui.New()
	in := []byte(`<script>alert(1)</script>`)
	assert.Equal(t, in, r.ServeTransform(in))
}

// TestDelivery_And_LiveView_KeepMcpuiOffTheGenericArtifactPipeline pins the
// two settings that keep an mcpui artifact out of the generic artifact
// live-view: BundledOnly delivery (never the top-level document) and
// SupportsLiveView()==false (artifact_offer_view refuses it outright).
func TestDelivery_And_LiveView_KeepMcpuiOffTheGenericArtifactPipeline(t *testing.T) {
	r := mcpui.New()
	assert.Equal(t, channelassets.DeliveryBundledOnly, r.Delivery())
	assert.False(t, r.SupportsLiveView())
}

func TestOutputMIMEs(t *testing.T) {
	assert.Equal(t, []string{"text/html"}, mcpui.New().OutputMIMEs())
}
