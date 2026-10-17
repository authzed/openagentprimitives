package browser

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"

	"github.com/chromedp/cdproto/dom"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactPNG_CoversBoxesAndLeavesRest(t *testing.T) {
	// 20x20 image, a 10x10 red "secret" block at (0,0), blue elsewhere.
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	red, blue := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{blue}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, 0, 10, 10), &image.Uniform{red}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))

	out, err := redactPNG(buf.Bytes(), []image.Rectangle{image.Rect(0, 0, 10, 10)})
	require.NoError(t, err)
	got, err := png.Decode(bytes.NewReader(out))
	require.NoError(t, err)
	// Inside the box: NOT red anymore (covered).
	r, g, b, _ := got.At(5, 5).RGBA()
	assert.False(t, r == 0xffff && g == 0 && b == 0, "secret pixel must be covered, got r=%d g=%d b=%d", r, g, b)
	// Outside the box: still blue (untouched).
	r2, g2, b2, _ := got.At(15, 15).RGBA()
	assert.True(t, r2 == 0 && g2 == 0 && b2 == 0xffff, "non-secret pixel must be untouched")
}

func TestRedactPNG_RejectsBadInput(t *testing.T) {
	_, err := redactPNG([]byte("not a png"), nil)
	require.Error(t, err)
}

// Review Focus: a device-pixel-ratio > 1 means CDP screenshot pixels are
// dpr * CSS box pixels, so the redaction rect must be scaled or it covers the
// wrong region. (boxFromModelScaled is implemented in Step 5; write this test
// alongside it.)
func TestBoxFromModelScaled_AppliesDPR(t *testing.T) {
	// A CSS box at (10,20)-(30,50); at dpr=2 the screenshot rect is doubled.
	b := &dom.BoxModel{Content: []float64{10, 20, 30, 20, 30, 50, 10, 50}}
	got := boxFromModelScaled(b, 2)
	assert.Equal(t, image.Rect(20, 40, 60, 100), got)
	// dpr=1 is identity.
	assert.Equal(t, image.Rect(10, 20, 30, 50), boxFromModelScaled(b, 1))
	// A fractional box must round OUTWARD (floor mins, ceil maxs) so the rect
	// fully encloses the field — never one pixel short on a scaled edge.
	frac := &dom.BoxModel{Content: []float64{5.4, 7.2, 10.6, 7.2, 10.6, 12.8, 5.4, 12.8}}
	assert.Equal(t, image.Rect(5, 7, 11, 13), boxFromModelScaled(frac, 1))
	// A degenerate box (too few quad points) is empty → caller fails closed.
	assert.True(t, boxFromModelScaled(&dom.BoxModel{}, 1).Empty())
}
