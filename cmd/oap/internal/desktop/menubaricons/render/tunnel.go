package render

import (
	"image"
	"math"

	"golang.org/x/image/vector"
)

// The "tunnel" setup motion (design-lab Z1/M5, 2026-09-16). One cycle, two
// phases, ease in-out:
//
//	phase A: a hole grows from the centroid until the outer is a ring of
//	         tunnelOutline of the half-width (the mark's hollow state);
//	phase B: a new solid grows from the centroid inside the hole until it
//	         fills it (back to the solid mark).
//
// Every frame is the same rounded triangle at three scales about the
// centroid, drawn evenodd: outer minus hole plus inner. The cycle is phased so
// frame 0 is the real logomark (small hole): the animation starts and ends on
// the mark, and the animator's first frame (also shown while idle) is the mark.
const (
	tunnelOutline = 0.38 // ring thickness as a fraction of the mark's half-extent
	// markCornerRadius is the mark's corner radius in design units: 15.2 on a
	// 366.5-wide sharp-vertex triangle, on a 19-unit base here.
	markCornerRadius = 0.79
	// menuMarkCutout is the cut-out of the small-size mark
	// (docs/assets/brand/oap-menu-icon-*.svg, 2026-09-23 finals) as a fraction
	// of the outer triangle, sharp vertex to sharp vertex: 22.84 on 53.12. The
	// full-size icon's is 0.336 (123.1 on 366.5); the menu variant opens the
	// hole up so it still reads at 16pt, and that is the size this renders at.
	menuMarkCutout = 0.43
)

// tunnelIdlePhase is where in the cycle the real mark sits, so frame 0 (and the
// running icon) is the menu mark: the phase-A point whose hole scale is
// menuMarkCutout, found by inverting the ease.
var tunnelIdlePhase = 0.5 * easeInOutInverse(menuMarkCutout/(1-tunnelOutline))

// easeInOut is the quadratic ease used by the design boards.
func easeInOut(u float64) float64 {
	if u < 0.5 {
		return 2 * u * u
	}
	return 1 - math.Pow(-2*u+2, 2)/2
}

// easeInOutInverse returns the u in [0,1] with easeInOut(u) == y.
func easeInOutInverse(y float64) float64 {
	if y < 0.5 {
		return math.Sqrt(y / 2)
	}
	return 1 - math.Sqrt(2*(1-y))/2
}

// tunnelFrame renders frame i of n of the tunnel cycle.
func tunnelFrame(i, n int) *image.Alpha {
	u := math.Mod(float64(i)/float64(n)+tunnelIdlePhase, 1)
	return tunnelShape(tunnelScales(u))
}

// tunnelScales returns the hole and inner-solid scales at cycle position u.
func tunnelScales(u float64) (hole, inner float64) {
	full := 1 - tunnelOutline
	if u < 0.5 {
		return full * easeInOut(u/0.5), 0
	}
	return full, full * easeInOut((u-0.5)/0.5)
}

// tunnelShape composes outer − hole + inner as alpha coverage.
func tunnelShape(hole, inner float64) *image.Alpha {
	dst := roundedTri(1)
	if hole > 0.01 {
		h := roundedTri(hole)
		for i := range dst.Pix {
			v := int(dst.Pix[i]) - int(h.Pix[i])
			if v < 0 {
				v = 0
			}
			dst.Pix[i] = uint8(v)
		}
	}
	if inner > 0.01 {
		if inner > hole-0.001 {
			inner = hole - 0.001
		}
		dst = unionAlpha(dst, roundedTri(inner))
	}
	return dst
}

// roundedTri rasterizes the mark's rounded triangle scaled by s about the
// centroid. The corner radius follows the mark (r = markCornerRadius·s^0.6, so
// a cut-out at the full-size icon's s≈0.33 has its ~half radius; at the menu
// mark's 0.43 that gives 0.60 where the menu SVG has 0.73, a 0.14px gap at 32px).
func roundedTri(s float64) *image.Alpha {
	dst := image.NewAlpha(image.Rect(0, 0, canvas, canvas))
	g := centroid()
	v := []pt{scalePt(apex, g, s), scalePt(br, g, s), scalePt(bl, g, s)}
	edge := math.Hypot(v[1].x-v[0].x, v[1].y-v[0].y)
	r := markCornerRadius * math.Pow(s, 0.6)
	d := math.Min(r/math.Tan(math.Pi/6), edge*0.45) // tangent distance from each vertex
	z := vector.NewRasterizer(canvas, canvas)
	for i := 0; i < 3; i++ {
		p, prev, next := v[i], v[(i+2)%3], v[(i+1)%3]
		t1 := towards(p, prev, d)
		t2 := towards(p, next, d)
		x1, y1 := t1.px()
		if i == 0 {
			z.MoveTo(x1, y1)
		} else {
			z.LineTo(x1, y1)
		}
		cx, cy := p.px()
		x2, y2 := t2.px()
		z.QuadTo(cx, cy, x2, y2)
	}
	z.ClosePath()
	z.Draw(dst, dst.Bounds(), image.Opaque, image.Point{})
	return dst
}

// towards returns the point d units from p along the direction to q.
func towards(p, q pt, d float64) pt {
	l := math.Hypot(q.x-p.x, q.y-p.y)
	return pt{p.x + (q.x-p.x)/l*d, p.y + (q.y-p.y)/l*d}
}
