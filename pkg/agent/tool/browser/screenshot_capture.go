package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"log/slog"
	"math"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
)

// errString renders err for a structured log field: "" (never nil-as-text)
// when err is nil, so the diagnostic line's "err" field is grep-friendly on
// both the success and failure path.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

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
//
// When blanked is empty — no credential field is live on the page, the common
// case outside a login flow — it takes a FAST PATH: the device-pixel-ratio
// read and the GetBoxModel loop below exist only to scale and locate blanked
// fields, so with none to redact it skips straight to CaptureScreenshot. That
// is one fewer CDP round trip (the DPR eval) on every non-redaction capture,
// which is also the common case the Task #53 desktop run intermittently timed
// out on — see the INFO log below for the per-stage timing that pins which
// stage, on a given run, actually used the budget.
func (s *Session) CaptureRedacted(ctx context.Context) ([]byte, error) {
	var shot []byte
	var boxes []image.Rectangle
	var dprDur, boxesDur, captureDur time.Duration
	var blanked []int64
	var fastPath bool

	timing, runErr := s.doTimed(ctx, redactedScreenshotTimeout, func(c context.Context) error {
		// Read the blanked set INSIDE the critical section. A type/code op marks
		// its field blanked inside its OWN do() closure, serialized with this one
		// by the session semaphore, so reading here — not before acquiring the
		// semaphore — guarantees we never miss a field a just-completed type
		// landed a credential in. (TOCTOU: reading it before the semaphore let a
		// concurrent type land a credential whose node was not yet in the blanked
		// set, shipping it unredacted in a human-visible, persisted approval
		// image.)
		blanked = s.blankedNodes()
		fastPath = len(blanked) == 0
		if fastPath {
			captureStart := time.Now()
			ae := runAction(c, func(ac context.Context) error {
				var e error
				shot, e = page.CaptureScreenshot().Do(ac)
				return e
			})
			captureDur = time.Since(captureStart)
			return ae
		}

		// Device pixel ratio: CaptureScreenshot pixels are DPR * CSS pixels,
		// while GetBoxModel returns CSS pixels, so boxes must be scaled.
		var dpr float64 = 1
		dprStart := time.Now()
		ae := runAction(c, func(ac context.Context) error {
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
		})
		dprDur = time.Since(dprStart)
		if ae != nil {
			return ae
		}

		boxesStart := time.Now()
		for _, id := range blanked {
			var box *dom.BoxModel
			if ae := runAction(c, func(ac context.Context) error {
				var e error
				box, e = dom.GetBoxModel().WithBackendNodeID(cdp.BackendNodeID(id)).Do(ac)
				return e
			}); ae != nil {
				boxesDur = time.Since(boxesStart)
				// Fail closed: a blanked field we can't locate means we can't
				// prove the screenshot is clean.
				return fmt.Errorf("redact: box for blanked node %d: %w", id, ae)
			}
			// An empty rect (a non-nil box with too few quad points) is also a
			// resolution failure: we cannot prove what region holds the secret,
			// so fail closed rather than ship an unredacted field.
			if r := boxFromModelScaled(box, dpr); r.Empty() {
				boxesDur = time.Since(boxesStart)
				return fmt.Errorf("redact: empty box for blanked node %d", id)
			} else {
				boxes = append(boxes, r)
			}
		}
		boxesDur = time.Since(boxesStart)

		captureStart := time.Now()
		ae = runAction(c, func(ac context.Context) error {
			var e error
			shot, e = page.CaptureScreenshot().Do(ac)
			return e
		})
		captureDur = time.Since(captureStart)
		return ae
	})

	// INFO, not Debug: on both success and the timeout path, so the ephemeral
	// browser pod's logs reveal which stage was slow on the next desktop run
	// (Task #53) — the pod is gone by the time anyone goes looking otherwise.
	slog.Info("browser screenshot: CaptureRedacted stage timing",
		"toolbox", s.logName,
		"op", "capture_redacted",
		"blanked_count", len(blanked),
		"fast_path", fastPath,
		"lock_wait_ms", timing.LockWait.Milliseconds(),
		"ensure_connected_ms", timing.EnsureConnected.Milliseconds(),
		"run_ms", timing.Run.Milliseconds(),
		"dpr_eval_ms", dprDur.Milliseconds(),
		"box_resolve_ms", boxesDur.Milliseconds(),
		"capture_screenshot_ms", captureDur.Milliseconds(),
		"ok", runErr == nil,
		"err", errString(runErr),
	)

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
