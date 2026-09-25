package render

import (
	"bytes"
	"image"
	"image/png"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons"
)

func TestRenderFramesCounts(t *testing.T) {
	require.Len(t, RenderFrames(menubaricons.StateRunning), 1)
	require.Len(t, RenderFrames(menubaricons.StateStopped), 1)
	require.Len(t, RenderFrames(menubaricons.StateError), 1)
	require.Len(t, RenderFrames(menubaricons.StateSetup), 12)
	require.Len(t, RenderFrames(menubaricons.StateShuttingDown), 12)
	require.Len(t, RenderFrames(menubaricons.StateUninstalling), 12)
	require.Len(t, RenderFrames(menubaricons.StateQuitting), 12)
}

func TestRenderFramesAre32pxPNG(t *testing.T) {
	for _, s := range menubaricons.AllStates() {
		for i, b := range RenderFrames(s) {
			img, err := png.Decode(bytes.NewReader(b))
			require.NoErrorf(t, err, "%s frame %d decode", s.Basename(), i)
			require.Equal(t, 32, img.Bounds().Dx())
			require.Equal(t, 32, img.Bounds().Dy())
		}
	}
}

func TestRunningFrameHasInk(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(RenderFrames(menubaricons.StateRunning)[0]))
	require.NoError(t, err)
	na, ok := img.(*image.NRGBA)
	require.True(t, ok)
	ink := 0
	for i := 3; i < len(na.Pix); i += 4 {
		if na.Pix[i] > 0 {
			ink++
		}
	}
	require.Greater(t, ink, 50, "running template should have opaque pixels")
}

func TestSetupFramesDiffer(t *testing.T) {
	f := RenderFrames(menubaricons.StateSetup)
	require.False(t, bytes.Equal(f[0], f[6]), "setup animation must change across frames")
}

func TestFramesStayWithinCanvas(t *testing.T) {
	// No frame's shape should reach the outer 1px border. Ink there means a
	// motion pushed geometry off-canvas and the rasterizer clamped/cropped it.
	for _, s := range menubaricons.AllStates() {
		for fi, b := range RenderFrames(s) {
			img, err := png.Decode(bytes.NewReader(b))
			require.NoError(t, err)
			na, ok := img.(*image.NRGBA)
			require.True(t, ok)
			const w, h = 32, 32
			ink := func(x, y int) bool { return na.Pix[(y*w+x)*4+3] > 8 }
			for x := 0; x < w; x++ {
				require.Falsef(t, ink(x, 0), "%s frame %d touches top border at x=%d", s.Basename(), fi, x)
				require.Falsef(t, ink(x, h-1), "%s frame %d touches bottom border at x=%d", s.Basename(), fi, x)
			}
			for y := 0; y < h; y++ {
				require.Falsef(t, ink(0, y), "%s frame %d touches left border at y=%d", s.Basename(), fi, y)
				require.Falsef(t, ink(w-1, y), "%s frame %d touches right border at y=%d", s.Basename(), fi, y)
			}
		}
	}
}

func TestTemplateRGBIsZero(t *testing.T) {
	// Template images use only the alpha channel; RGB must be 0 so macOS tints cleanly.
	img, err := png.Decode(bytes.NewReader(RenderFrames(menubaricons.StateRunning)[0]))
	require.NoError(t, err)
	na, ok := img.(*image.NRGBA)
	require.True(t, ok)
	for i := 0; i < len(na.Pix); i += 4 {
		require.Zerof(t, na.Pix[i], "R at %d must be 0", i)
		require.Zerof(t, na.Pix[i+1], "G at %d must be 0", i)
		require.Zerof(t, na.Pix[i+2], "B at %d must be 0", i)
	}
}

func TestSetupStartsAndEndsOnTheMark(t *testing.T) {
	// The tunnel cycle is phased so frame 0 is the logomark: solid with a small
	// hole at the centroid. The animator shows frame 0 first and idles on it.
	f := RenderFrames(menubaricons.StateSetup)
	alphaAt := func(b []byte, x, y int) uint8 {
		img, err := png.Decode(bytes.NewReader(b))
		require.NoError(t, err)
		return img.(*image.NRGBA).Pix[(y*32+x)*4+3]
	}
	// centroid of the design triangle ≈ (12, 15.2) units → (16, 20) px
	require.Less(t, alphaAt(f[0], 16, 20), uint8(64), "frame 0 must have the mark's hole at the centroid")
	require.Greater(t, alphaAt(f[0], 16, 26), uint8(200), "frame 0 must be solid below the hole")
	// Locate the hollowest (u = 0.5) and fully refilled (u = 1) frames from the
	// idle phase rather than by index, so retuning the mark does not move them.
	frameAt := func(u float64) int { return int(math.Round((u-tunnelIdlePhase)*float64(len(f)))) % len(f) }
	require.Less(t, alphaAt(f[frameAt(0.5)], 16, 14), uint8(32), "mid-cycle frame must be hollow (ring)")
	require.Greater(t, alphaAt(f[frameAt(1)], 16, 20), uint8(200), "late frame must be refilled at the centroid")
	// Running (idle) is that same mark, so setup settles into it without a jump.
	require.True(t, bytes.Equal(f[0], RenderFrames(menubaricons.StateRunning)[0]), "running must equal setup frame 0")
}

func TestIdleFrameIsMenuMark(t *testing.T) {
	// The running icon and the setup loop's first frame are the menu mark: the
	// tunnel parks with its hole at the menu SVG's cut-out and no inner solid.
	hole, inner := tunnelScales(tunnelIdlePhase)
	assert.InDelta(t, menuMarkCutout, hole, 1e-9)
	assert.Zero(t, inner)
	assert.Equal(t, tunnelFrame(0, 1).Pix, tunnelFrame(0, menubaricons.StateSetup.FrameCount()).Pix)
}
