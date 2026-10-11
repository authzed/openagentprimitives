package browser

// screenshot_capture_chrome_test.go is the real redaction proof against a live
// Chrome (startFixture, chrome_test.go): serve a page with a solid-red text
// input, type a credential into it through the REAL fill path so the field is
// marked blanked, call CaptureRedacted, and prove the field's region is painted
// black in the returned PNG while the rest of the page is untouched. Skipped
// with a reason when no Chrome binary is present (spec §7).

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// redactSecretPage serves a single text input painted solid red (both
// background and text color) and absolutely positioned at a known spot, large
// enough that its box is unambiguous. A credential typed into it is therefore a
// red rectangle in the screenshot; after redaction it must be a BLACK
// rectangle. The page background is explicit white so a control sample outside
// the field proves the redaction was targeted, not a blanket blackout.
func redactSecretPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><head><style>` +
			`html,body{margin:0;padding:0;background:#ffffff;}` +
			`#secret{position:absolute;left:40px;top:40px;width:240px;height:60px;` +
			`padding:0;background:#ff0000;color:#ff0000;border:0;outline:0;font-size:24px;}` +
			`</style></head><body>` +
			`<form><input id="secret" type="text" aria-label="SecretField"></form>` +
			`</body></html>`))
	})
}

// browserRedactFixture boots the chrome fixture in act mode with a one-entry
// catalog for the fixture site, returning the synthesized tools, the shared
// session (for the blanked set + box resolution), and the site base URL.
func browserRedactFixture(t *testing.T) (tools []agenttool.Tool, sess *Session, siteBase string) {
	t.Helper()
	fx := startFixture(t, redactSecretPage())
	fx.rt.Spec.Interaction = "act"
	fx.rt.IdentityName = "site-identity"
	fx.rt.Catalog = []spiceboxv1alpha1.BrowserLoginRef{{
		ID:    "login-site",
		Label: "Site",
		Hosts: []spiceboxv1alpha1.BrowserLoginHost{{Host: "site.test"}},
	}}
	ff := &fakeFetcher{}
	var err error
	tools, err = Synthesize(fx.rt, fx.gateURL, chromeToken, false, ff.fetch(), nil, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, tools)
	sess = tools[0].(*browserTool).sess
	t.Cleanup(func() { sess.Close() })
	return tools, sess, fx.siteBase
}

// TestCaptureRedacted_CoversBlankedField is the Task 2 keystone: a credential is
// typed into a field through the real fill path (which marks it blanked), and a
// subsequent CaptureRedacted paints that field's region solid black — proving no
// typed secret survives into the screenshot bytes — while leaving the rest of the
// page untouched.
func TestCaptureRedacted_CoversBlankedField(t *testing.T) {
	tools, sess, siteBase := browserRedactFixture(t)

	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": siteBase + "/"}).IsError)

	secretRef := refByName(t, tools, "SecretField")

	// Type a credential placeholder into the field via the REAL fill path; this
	// marks the field blanked exactly like any filled credential.
	typed := run(t, toolBy(t, tools, "type"), map[string]any{"ref": secretRef, "text": "{{login.site.test.username}}"})
	require.False(t, typed.IsError, "type into secret field: %s", typed.Content)
	require.Len(t, sess.blankedNodes(), 1, "the typed-into field must be marked blanked")

	// Resolve the field's screenshot-space rect the same way CaptureRedacted does
	// (content box scaled by DPR), so the sample points land inside the redacted
	// region on a HiDPI display as well.
	entry, ok := sess.lookupRef(secretRef)
	require.True(t, ok, "the typed ref must still resolve (no navigation)")
	rect := fieldScreenshotRect(t, sess, entry.backendNodeID)
	require.False(t, rect.Empty(), "the field box must resolve to a non-empty rect")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	shot, err := sess.CaptureRedacted(ctx)
	require.NoError(t, err, "CaptureRedacted must succeed with a resolvable blanked field")
	require.NotEmpty(t, shot, "a redacted screenshot must have bytes")

	img, err := png.Decode(bytes.NewReader(shot))
	require.NoError(t, err)

	// Every sampled pixel across the field's region must be opaque black — the red
	// secret rectangle is gone (the brief's uniform-fill-color assertion).
	for _, p := range samplePoints(rect) {
		r, g, b, a := img.At(p.X, p.Y).RGBA()
		assert.True(t, r == 0 && g == 0 && b == 0 && a == 0xffff,
			"field pixel (%d,%d) must be redacted black, got r=%d g=%d b=%d a=%d", p.X, p.Y, r, g, b, a)
	}

	// Control: a pixel at the top-left page background, well outside the field,
	// must be untouched white — redaction is targeted, not a blanket blackout.
	cr, cg, cb, _ := img.At(2, 2).RGBA()
	assert.True(t, cr == 0xffff && cg == 0xffff && cb == 0xffff,
		"the page background outside the field must be untouched, got r=%d g=%d b=%d", cr, cg, cb)
}

// fieldScreenshotRect resolves backendNodeID's content box and scales it to
// screenshot pixels exactly as CaptureRedacted does, so a test's sample points
// match the production redacted region regardless of device pixel ratio.
func fieldScreenshotRect(t *testing.T, sess *Session, backendNodeID int64) image.Rectangle {
	t.Helper()
	var rect image.Rectangle
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, sess.do(ctx, shortTimeout, func(c context.Context) error {
		var dpr float64 = 1
		if err := runAction(c, func(ac context.Context) error {
			r, _, e := runtime.Evaluate("window.devicePixelRatio").Do(ac)
			if e == nil && r != nil && r.Value != nil {
				_ = json.Unmarshal(r.Value, &dpr)
			}
			return nil
		}); err != nil {
			return err
		}
		var box *dom.BoxModel
		if err := runAction(c, func(ac context.Context) error {
			var e error
			box, e = dom.GetBoxModel().WithBackendNodeID(cdp.BackendNodeID(backendNodeID)).Do(ac)
			return e
		}); err != nil {
			return err
		}
		rect = boxFromModelScaled(box, dpr)
		return nil
	}))
	return rect
}

// captureSlogOutput temporarily redirects the slog default logger to a text
// handler writing into a buffer, runs fn, restores the prior default, and
// returns everything logged. Used below to observe CaptureRedacted's own
// INFO diagnostics (Task #53) rather than re-deriving them out-of-band — the
// log line IS the fast-path proof: "box_resolve_ms=0"/"dpr_eval_ms=0" can
// only appear if those stages were never entered, since the vars they'd set
// start zero-valued and are only assigned inside the (skipped) slow path.
func captureSlogOutput(t *testing.T, fn func()) string {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return buf.String()
}

// TestCaptureRedacted_EmptyBlankedTakesFastPath pins the Task #53 robustness
// fix: when no credential field is blanked (the common case outside a login
// flow), CaptureRedacted must skip the device-pixel-ratio eval and the
// GetBoxModel loop entirely — one fewer CDP round trip, less to time out on a
// cold or busy browser pod — and still return a valid, un-redacted
// screenshot. The INFO diagnostic line CaptureRedacted emits is the proof:
// dpr_eval_ms and box_resolve_ms can only read 0 if those stages' bodies
// never ran (their timers are started and stopped only inside each branch).
func TestCaptureRedacted_EmptyBlankedTakesFastPath(t *testing.T) {
	tools, sess, siteBase := browserRedactFixture(t)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": siteBase + "/"}).IsError)
	require.Empty(t, sess.blankedNodes(), "nothing was typed into the secret field; blanked must be empty")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var shot []byte
	logOut := captureSlogOutput(t, func() {
		var err error
		shot, err = sess.CaptureRedacted(ctx)
		require.NoError(t, err, "an empty-blanked capture must still succeed")
	})

	require.NotEmpty(t, shot, "the fast path must still return screenshot bytes")
	_, decErr := png.Decode(bytes.NewReader(shot))
	require.NoError(t, decErr, "the fast-path bytes must be a valid PNG")

	assert.Contains(t, logOut, "fast_path=true", "the empty-blanked fast path must be taken")
	assert.Contains(t, logOut, "blanked_count=0")
	assert.Contains(t, logOut, "dpr_eval_ms=0", "the fast path must skip the device-pixel-ratio round trip entirely")
	assert.Contains(t, logOut, "box_resolve_ms=0", "the fast path must skip GetBoxModel entirely — nothing to redact")
	assert.Contains(t, logOut, "ok=true")
}

// TestCaptureRedacted_BlankedFieldTakesSlowPath is the fast path's negative
// control: with a blanked field present, the SAME diagnostic line must show
// the DPR eval and box-model stages were actually entered (non-fast-path),
// so the fast-path test above is proving a real branch, not a vacuous one.
func TestCaptureRedacted_BlankedFieldTakesSlowPath(t *testing.T) {
	tools, sess, siteBase := browserRedactFixture(t)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": siteBase + "/"}).IsError)

	secretRef := refByName(t, tools, "SecretField")
	typed := run(t, toolBy(t, tools, "type"), map[string]any{"ref": secretRef, "text": "{{login.site.test.username}}"})
	require.False(t, typed.IsError, "type into secret field: %s", typed.Content)
	require.Len(t, sess.blankedNodes(), 1, "the typed-into field must be marked blanked")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	logOut := captureSlogOutput(t, func() {
		_, err := sess.CaptureRedacted(ctx)
		require.NoError(t, err, "a resolvable blanked field must still succeed")
	})

	assert.Contains(t, logOut, "fast_path=false", "a non-empty blanked set must take the full redaction path")
	assert.Contains(t, logOut, "blanked_count=1")
	assert.Contains(t, logOut, "ok=true")
}

// samplePoints returns the center plus the four inset corners of r, staying a
// few pixels inside each edge to avoid border antialiasing.
func samplePoints(r image.Rectangle) []image.Point {
	const inset = 4
	minX, minY := r.Min.X+inset, r.Min.Y+inset
	maxX, maxY := r.Max.X-inset-1, r.Max.Y-inset-1
	cx, cy := (r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2
	return []image.Point{
		{X: cx, Y: cy},
		{X: minX, Y: minY},
		{X: maxX, Y: minY},
		{X: minX, Y: maxY},
		{X: maxX, Y: maxY},
	}
}
