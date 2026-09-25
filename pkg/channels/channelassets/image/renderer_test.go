package image_test

import (
	"bytes"
	"context"
	stdimage "image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	imgkind "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	m := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	m.Set(0, 0, color.RGBA{1, 2, 3, 255})
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, m))
	return b.Bytes()
}

func TestRender_PNG_StripsAppendedPolyglotBytes(t *testing.T) {
	src := append(pngBytes(t, 8, 8), []byte("<script>alert(1)</script>")...)
	out, err := imgkind.New().Render(context.Background(), channelassets.Input{Payload: src})
	require.NoError(t, err)
	assert.Equal(t, "image/png", out.MIME)
	assert.NotContains(t, string(out.Bytes), "<script>", "appended polyglot bytes must be gone after re-encode")
	_, err = png.Decode(bytes.NewReader(out.Bytes))
	assert.NoError(t, err, "output must be a valid PNG")
}

func TestRender_AnimatedGIF_PreservesFrames(t *testing.T) {
	g := &gif.GIF{}
	for i := 0; i < 3; i++ {
		p := stdimage.NewPaletted(stdimage.Rect(0, 0, 4, 4), []color.Color{color.Black, color.White})
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 10)
	}
	var b bytes.Buffer
	require.NoError(t, gif.EncodeAll(&b, g))
	out, err := imgkind.New().Render(context.Background(), channelassets.Input{Payload: b.Bytes()})
	require.NoError(t, err)
	assert.Equal(t, "image/gif", out.MIME)
	decoded, err := gif.DecodeAll(bytes.NewReader(out.Bytes))
	require.NoError(t, err)
	assert.Len(t, decoded.Image, 3, "all animation frames preserved")
}

func TestRender_OverDimension_Rejected(t *testing.T) {
	_, err := imgkind.New().Render(context.Background(), channelassets.Input{Payload: pngBytes(t, 5000, 10)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds max")
}

func TestRender_NonImage_Rejected(t *testing.T) {
	_, err := imgkind.New().Render(context.Background(), channelassets.Input{Payload: []byte("not an image")})
	require.Error(t, err)
}

func TestRendererContract(t *testing.T) {
	r := imgkind.New()
	assert.Equal(t, "image", r.Kind())
	assert.Equal(t, channelassets.DeliveryStandalone, r.Delivery())
	assert.False(t, r.SupportsLiveView())
	assert.ElementsMatch(t, []string{"image/png", "image/jpeg", "image/gif"}, r.OutputMIMEs())
}
