package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"math"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
)

// BrowserScreenshotCapturer captures a redacted viewport screenshot of the
// current page. Injected into the runner's interaction-publish paths; the
// browser session is the only implementation.
type BrowserScreenshotCapturer interface {
	CaptureRedacted(ctx context.Context) ([]byte, error)
}

// ScreenshotProvider is implemented by a tool whose live browser session can
// produce a redacted approval screenshot. The runner's approval-publish path
// (pkg/agent/runner) type-asserts a dispatched tool.Tool for this interface —
// the same optional-interface idiom NamedCallArgs uses (tools.go) to surface
// CurrentHost to the Permission check — rather than threading a capturer
// through a new Loop-level seam. Only *browserTool implements it; a
// non-browser tool therefore never yields a capturer and the runner never
// captures a screenshot for it.
type ScreenshotProvider interface {
	// ScreenshotCapturer returns the capturer for this tool's live session, or
	// nil if none is available (e.g. a browserTool synthesized without a
	// session in a unit test).
	ScreenshotCapturer() BrowserScreenshotCapturer
}

// CaptureRedacted captures the viewport and paints over every credential-filled
// (blanked) field. It is FAIL-CLOSED: if any blanked field's box cannot be
// resolved it returns an error and NO bytes, so the caller ships no screenshot
// rather than one that might reveal a typed secret.
func (s *Session) CaptureRedacted(ctx context.Context) ([]byte, error) {
	blanked := s.blankedNodes()
	var shot []byte
	var boxes []image.Rectangle
	runErr := s.do(ctx, snapshotTimeout, func(c context.Context) error {
		// Device pixel ratio: CaptureScreenshot pixels are DPR * CSS pixels,
		// while GetBoxModel returns CSS pixels, so boxes must be scaled.
		var dpr float64 = 1
		if ae := runAction(c, func(ac context.Context) error {
			r, _, err := runtime.Evaluate("window.devicePixelRatio").Do(ac)
			if err != nil {
				// Fail closed: without a trusted DPR we could compute boxes at 1x
				// against a 2x screenshot and UNDER-cover the field on a HiDPI
				// display. An eval error must ship no screenshot, like a box
				// resolution failure — not silently assume 1x.
				return fmt.Errorf("redact: read device pixel ratio: %w", err)
			}
			if r != nil && r.Value != nil {
				_ = json.Unmarshal(r.Value, &dpr) // best-effort; a weird value falls back below
			}
			if dpr <= 0 {
				dpr = 1
			}
			return nil
		}); ae != nil {
			return ae
		}
		for _, id := range blanked {
			var box *dom.BoxModel
			if ae := runAction(c, func(ac context.Context) error {
				var e error
				box, e = dom.GetBoxModel().WithBackendNodeID(cdp.BackendNodeID(id)).Do(ac)
				return e
			}); ae != nil {
				// Fail closed: a blanked field we can't locate means we can't
				// prove the screenshot is clean.
				return fmt.Errorf("redact: box for blanked node %d: %w", id, ae)
			}
			// An empty rect (a non-nil box with too few quad points) is also a
			// resolution failure: we cannot prove what region holds the secret,
			// so fail closed rather than ship an unredacted field.
			if r := boxFromModelScaled(box, dpr); r.Empty() {
				return fmt.Errorf("redact: empty box for blanked node %d", id)
			} else {
				boxes = append(boxes, r)
			}
		}
		if ae := runAction(c, func(ac context.Context) error {
			var e error
			shot, e = page.CaptureScreenshot().Do(ac)
			return e
		}); ae != nil {
			return ae
		}
		return nil
	})
	if runErr != nil {
		return nil, runErr
	}
	return redactPNG(shot, boxes)
}

// boxFromModelScaled converts a CDP content-quad box to an axis-aligned
// rectangle in screenshot pixels. The content quad is [x1,y1,x2,y2,x3,y3,x4,y4];
// we take the bounding rect and scale by the device pixel ratio.
func boxFromModelScaled(b *dom.BoxModel, dpr float64) image.Rectangle {
	if b == nil || len(b.Content) < 8 {
		return image.Rectangle{}
	}
	xs := []float64{b.Content[0], b.Content[2], b.Content[4], b.Content[6]}
	ys := []float64{b.Content[1], b.Content[3], b.Content[5], b.Content[7]}
	minX, maxX, minY, maxY := xs[0], xs[0], ys[0], ys[0]
	for _, x := range xs {
		if x < minX {
			minX = x
		}
		if x > maxX {
			maxX = x
		}
	}
	for _, y := range ys {
		if y < minY {
			minY = y
		}
		if y > maxY {
			maxY = y
		}
	}
	// Floor the min corners and ceil the max corners so the rect FULLY ENCLOSES
	// the field after DPR scaling — a fractional scaled edge (e.g. 560.4px) must
	// never leave a sub-pixel sliver of the credential field unpainted.
	return image.Rect(
		int(math.Floor(minX*dpr)), int(math.Floor(minY*dpr)),
		int(math.Ceil(maxX*dpr)), int(math.Ceil(maxY*dpr)),
	)
}
