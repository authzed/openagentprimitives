package browser

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// redactPNG decodes src (a PNG screenshot), paints each rect solid opaque
// black, and re-encodes. Rectangles are in SCREENSHOT PIXEL coordinates (the
// caller is responsible for device-pixel-ratio scaling). Returns an error on a
// decode/encode failure so the caller can fail closed (ship no screenshot)
// rather than ship a partially-redacted one.
func redactPNG(src []byte, boxes []image.Rectangle) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("redact: decode screenshot: %w", err)
	}
	// Draw onto an RGBA copy so we can composite regardless of the source type.
	out := image.NewRGBA(img.Bounds())
	draw.Draw(out, out.Bounds(), img, img.Bounds().Min, draw.Src)
	black := &image.Uniform{color.RGBA{0, 0, 0, 255}}
	for _, b := range boxes {
		r := b.Intersect(out.Bounds())
		if r.Empty() {
			continue
		}
		draw.Draw(out, r, black, image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("redact: re-encode screenshot: %w", err)
	}
	return buf.Bytes(), nil
}
