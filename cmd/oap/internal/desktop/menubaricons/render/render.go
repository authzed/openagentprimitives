// Package render draws the oap desktop menu-bar icon frames as macOS template
// PNGs (black shape, coverage in the alpha channel). It is used only at build
// time by the gen tool and by the assets golden test — never linked into the
// shipped oap binary. Imports x/image/vector for anti-aliased fills.
package render

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"math"

	"golang.org/x/image/vector"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons"
)

// canvas is the pixel size of every frame. systray sizes the NSImage to 16pt;
// 32px gives a crisp 2x (Retina) template. Design geometry is authored in a
// 24-unit space and scaled by canvas/designUnits.
const (
	canvas      = 32
	designUnits = 24.0
	scale       = canvas / designUnits
)

type pt struct{ x, y float64 }

func (p pt) px() (float32, float32) { return float32(p.x * scale), float32(p.y * scale) }

// Triangle vertices in the 24-unit design space (apex up).
var (
	apex = pt{12, 2.5}
	bl   = pt{2.5, 21.5}
	br   = pt{21.5, 21.5}
)

// outlineInsetFactor scales the triangle about its centroid to carve the
// stopped-state outline stroke.
const outlineInsetFactor = 0.74

// RenderFrames returns the PNG bytes for every frame of a state, index 0..N-1.
func RenderFrames(s menubaricons.State) [][]byte {
	n := s.FrameCount()
	out := make([][]byte, n)
	for i := 0; i < n; i++ {
		out[i] = encode(renderFrame(s, i, n))
	}
	return out
}

func renderFrame(s menubaricons.State, i, n int) *image.Alpha {
	switch s {
	case menubaricons.StateRunning:
		return tunnelFrame(0, 1) // the logomark: the tunnel's idle frame
	case menubaricons.StateStopped:
		return ringTri(0.62)
	case menubaricons.StateError:
		return errorTri()
	case menubaricons.StateShuttingDown:
		return flipFrame(i, n)
	case menubaricons.StateUninstalling:
		return shrinkFrame(i, n)
	case menubaricons.StateSetup:
		return tunnelFrame(i, n)
	case menubaricons.StateQuitting:
		return quittingFrame(i, n)
	default:
		return image.NewAlpha(image.Rect(0, 0, canvas, canvas))
	}
}

// rasterPolyInto fills the closed polygon `poly` into dst (coverage → alpha),
// compositing over whatever dst already holds so repeated calls accumulate.
func rasterPolyInto(dst *image.Alpha, poly []pt) {
	r := vector.NewRasterizer(canvas, canvas)
	x0, y0 := poly[0].px()
	r.MoveTo(x0, y0)
	for _, p := range poly[1:] {
		x, y := p.px()
		r.LineTo(x, y)
	}
	r.ClosePath()
	r.Draw(dst, dst.Bounds(), image.Opaque, image.Point{})
}

// rasterTriInto fills triangle a,b,c into dst (coverage → alpha).
func rasterTriInto(dst *image.Alpha, a, b, c pt) {
	rasterPolyInto(dst, []pt{a, b, c})
}

func fillTri(a, b, c pt, alpha float64) *image.Alpha {
	dst := image.NewAlpha(image.Rect(0, 0, canvas, canvas))
	rasterTriInto(dst, a, b, c)
	if alpha < 1 {
		scaleAlpha(dst, alpha)
	}
	return dst
}

func centroid() pt { return pt{(apex.x + bl.x + br.x) / 3, (apex.y + bl.y + br.y) / 3} }

func scalePt(p, c pt, s float64) pt { return pt{c.x + (p.x-c.x)*s, c.y + (p.y-c.y)*s} }

func insetTri() (pt, pt, pt) {
	g := centroid()
	return scalePt(apex, g, outlineInsetFactor), scalePt(bl, g, outlineInsetFactor), scalePt(br, g, outlineInsetFactor)
}

// ringTri renders the triangle outline (outer minus inset) at the given alpha.
func ringTri(alpha float64) *image.Alpha {
	outer := image.NewAlpha(image.Rect(0, 0, canvas, canvas))
	rasterTriInto(outer, apex, bl, br)
	ia, ib, ic := insetTri()
	inner := image.NewAlpha(image.Rect(0, 0, canvas, canvas))
	rasterTriInto(inner, ia, ib, ic)
	dst := image.NewAlpha(image.Rect(0, 0, canvas, canvas))
	for i := range dst.Pix {
		v := int(outer.Pix[i]) - int(inner.Pix[i])
		if v < 0 {
			v = 0
		}
		dst.Pix[i] = uint8(float64(v) * alpha)
	}
	return dst
}

// errorTri fills the broken-notch triangle (a V bitten out of the right edge).
func errorTri() *image.Alpha {
	dst := image.NewAlpha(image.Rect(0, 0, canvas, canvas))
	rasterPolyInto(dst, []pt{apex, {16.25, 11}, {13.8, 12.7}, {18, 14.5}, br, bl})
	return dst
}

func rot(p, c pt, ang float64) pt {
	s, co := math.Sin(ang), math.Cos(ang)
	dx, dy := p.x-c.x, p.y-c.y
	return pt{c.x + dx*co - dy*s, c.y + dx*s + dy*co}
}

// flipFit shrinks the shutdown-flip triangle about the canvas center so every
// rotation angle stays inside the 24-unit canvas. The full triangle's far
// vertices sit ~13.4 units from center; 0.80 pulls the max radius to ~10.7,
// under the 12-unit half-extent with margin, so no frame's ink reaches the
// outer border pixels (guarded by TestFramesStayWithinCanvas).
const flipFit = 0.80

// flipFrame animates the "shutting down" motion: the solid triangle rotates
// from pointing-up to pointing-down, fades out while down, then — resetting its
// orientation to pointing-up while invisible — fades back in and repeats. The
// triangle is scaled by flipFit first so no rotation angle pushes a vertex
// off-canvas (which would otherwise crop the tips against the edge).
func flipFrame(i, n int) *image.Alpha {
	f := float64(i) / float64(n)
	var ang, alpha float64
	switch {
	case f < 0.40: // rotate up → down, fully visible
		ang = math.Pi * (f / 0.40)
		alpha = 1
	case f < 0.62: // hold pointing down, fade out
		ang = math.Pi
		alpha = 1 - (f-0.40)/(0.62-0.40)
	case f < 0.70: // gone — reset orientation to up (ang 0) while invisible
		alpha = 0
	default: // pointing up, fade back in
		alpha = (f - 0.70) / (1.0 - 0.70)
	}
	c := pt{designUnits / 2, designUnits / 2}
	a := scalePt(apex, c, flipFit)
	b := scalePt(bl, c, flipFit)
	d := scalePt(br, c, flipFit)
	return fillTri(rot(a, c, ang), rot(b, c, ang), rot(d, c, ang), alpha)
}

// shrinkFrame scales the triangle toward the center while fading, then holds
// gone — the "uninstall / remove" motion.
func shrinkFrame(i, n int) *image.Alpha {
	f := float64(i) / float64(n)
	c := pt{designUnits / 2, designUnits / 2}
	sc, al := 0.1, 0.0
	if f < 0.55 {
		p := f / 0.55
		sc = 1 - 0.9*p
		al = 1 - p
	}
	return fillTri(scalePt(apex, c, sc), scalePt(bl, c, sc), scalePt(br, c, sc), al)
}

// quittingFrame fades a DOWNWARD-pointing (apex-down) triangle toward
// transparent while it drifts gently downward, then holds gone — the "app
// closing" motion shown briefly after the VM has stopped, just before the
// process exits. It points down (a 180° rotation of the running triangle) so
// the icon matches the already-shut-off state rather than the pointing-up
// "live" shape.
func quittingFrame(i, n int) *image.Alpha {
	f := float64(i) / float64(n)
	alpha := 1 - f/0.6 // fully faded by ~60% of the loop
	if alpha < 0 {
		alpha = 0
	}
	const sinkMax = 1.6 // design units of downward drift while fading
	dy := sinkMax * f
	c := pt{designUnits / 2, designUnits / 2}
	down := func(q pt) pt {
		d := rot(q, c, math.Pi) // flip apex-up → apex-down
		return pt{d.x, d.y + dy}
	}
	return fillTri(down(apex), down(bl), down(br), alpha)
}

func unionAlpha(a, b *image.Alpha) *image.Alpha {
	dst := image.NewAlpha(a.Bounds())
	for i := range dst.Pix {
		v := a.Pix[i]
		if b.Pix[i] > v {
			v = b.Pix[i]
		}
		dst.Pix[i] = v
	}
	return dst
}

func scaleAlpha(img *image.Alpha, a float64) {
	for i := range img.Pix {
		img.Pix[i] = uint8(float64(img.Pix[i]) * a)
	}
}

// encode builds a black NRGBA whose alpha is the coverage, then PNG-encodes it
// (macOS template images use the alpha channel; RGB is ignored).
func encode(a *image.Alpha) []byte {
	rgba := image.NewNRGBA(a.Bounds())
	for i, cov := range a.Pix {
		rgba.Pix[i*4+3] = cov // rgb stays 0 (black)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		panic(fmt.Sprintf("menubaricons/render: encode PNG: %v", err))
	}
	return buf.Bytes()
}
