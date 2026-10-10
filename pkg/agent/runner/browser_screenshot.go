package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	browsertool "github.com/authzed/openagentprimitives/pkg/agent/tool/browser"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memartifact "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// screenshotInlineMaxBytes is the ArtifactRenderSpec.Payload inline cap
// (pkg/apis/v1alpha1/artifactrender_types.go:57-65: "Payload inline, ≤256
// KiB. Exactly one of Payload or PayloadRef MUST be set."). A redacted
// screenshot at or under this size is attached inline; a larger one is
// written to the artifact store first and referenced by PayloadRef.
const screenshotInlineMaxBytes = 256 << 10

// Fixed metadata for every screenshot artifact this package creates: one
// filename/alt-text for the whole feature, so it only needs saying once.
// The renderer kind and MIME are NOT literals here — see
// channelevents.ScreenshotRendererKind / ScreenshotMIME for why (this
// package is under mime_opinion_guard_test.go).
const (
	screenshotFilename = "page.png"
	screenshotAltText  = "current page (credential fields redacted)"
)

// captureScreenshotTimeout is the best-effort capture's OWN deadline —
// deliberately decoupled from whatever deadline the caller's ctx carries
// (see captureBrowserScreenshotFn's doc). It must be generous enough to
// survive a cold first capture against a real browser pod: a credentialed
// desktop run observed the capture losing the race against an ask-raise /
// dispatch ctx whose deadline was far tighter than the capturer's own 20s
// (now browser.redactedScreenshotTimeout's 25s) internal budget, logging
// "capture failed; publishing text-only" / "context deadline exceeded" on
// every approval. 30s leaves headroom above that 25s inner budget for the
// storeScreenshotArtifact call (a k8s Create plus a short poll-to-Ready and
// finalize, bounded by screenshotRenderTimeout below) that follows it.
const captureScreenshotTimeout = 30 * time.Second

// screenshotRenderPollInterval / screenshotRenderTimeout bound
// awaitScreenshotReady's wait for the just-created ArtifactRender to reach
// Ready, mirroring uiResourcePollInterval/uiResourceRenderTimeout in
// loop_uiresource.go — but with a tighter ceiling, because this wait has to
// fit inside captureScreenshotTimeout's budget alongside the capture that
// precedes it. The "image" renderer (pkg/channels/channelassets/image) runs
// in-process with no external I/O, so Ready lands within one reconcile tick
// in practice; 5s is a safety bound, not an expected wait.
const (
	screenshotRenderPollInterval = 100 * time.Millisecond
	screenshotRenderTimeout      = 5 * time.Second
)

// screenshotArtifactPutter is the subset of artifactstore.Store that
// storeScreenshotArtifact needs: Put, to store a screenshot too large to
// inline. Narrowed (mirroring secretOutputStatusWriter in loop_secretout.go
// and uiResourceStatusWriter in loop_uiresource.go) so a test can provide a
// minimal recorder without constructing a full artifactstore.Store.
type screenshotArtifactPutter interface {
	Put(ctx context.Context, key string, r io.Reader) (artifactstore.Ref, error)
}

// screenshotStoreDeps bundles the backend wiring storeScreenshotArtifact
// needs to turn redacted screenshot bytes into a durable ArtifactRender CR:
// the CR client, the artifact store (consulted only when the PNG exceeds the
// inline cap), and the session's UID for the CR's owner-ref.
//
// NewRenderName and NewArtifactID are optional id seams: nil means
// production (artifacts.RenderName and memory.NewID(memartifact.Kind{}),
// the same pair artifacts.Service itself defaults to) — mirroring
// artifacts.Service's own WithRenderNameMinter/newID seams so tests can
// assert on a deterministic CR name and artifact id instead of a random one.
type screenshotStoreDeps struct {
	Client        client.Client
	ArtifactStore screenshotArtifactPutter
	SessionUID    types.UID

	// Artifacts finalizes the ArtifactRender once Ready — the same
	// *artifacts.Service applyUIResource's uiResourceDeps.Artifacts uses. See
	// awaitScreenshotReady for why this is required, not optional bookkeeping.
	Artifacts *artifacts.Service

	NewRenderName func(session string) string
	NewArtifactID func() string
}

// storeScreenshotArtifact persists a redacted page screenshot (the PNG bytes
// Task 2 produces) as an ArtifactRender{Kind:"image"} CR, finalizes it once
// Ready, and returns the AttachmentRef the interaction request payload's
// Screenshot field carries.
//
// Mirrors the CR-build shape in pkg/agent/tool/meta/artifact_prepare.go:264-274
// and pkg/platform/artifacts.NewRender: owned by the session (by UID, as
// controller, blocking deletion), labeled with the artifact id.
//
// It DOES poll the CR to Ready and call FinalizeRevision (awaitScreenshotReady,
// below) — unlike an earlier version of this function, which skipped that step
// on the theory that a screenshot attaches to a live interaction request, not
// a model-facing handle, so "there is nothing to wait on." That theory missed
// that FinalizeRevision is also what makes the artifact VIEWABLE at all:
// pkg/memory/spicedbauthorizer.Authorizer writes the
// artifact:<id>#parent@agentsession:<ns>/<name> SpiceDB relationship the
// moment FinalizeRevision Puts the artifact's memory head entry, and that
// relationship is what artifact:<id>#view (parent->interact + ...) resolves
// against. Skipping FinalizeRevision left the screenshot's artifactID with no
// SpiceDB parent and no revision_of memory link, so
// pkg/web/webui/artifactview.bindView's CheckView/ListRevisions denied every
// request — the chat UI's <img src="/artifact-download?artifactId=...">
// (pkg/web/webui/chat/ui/InteractionCard.tsx) 403'd and failed to load even
// though the ArtifactRender CR itself reported Ready. See Task #54.
//
// A PNG at or under screenshotInlineMaxBytes is attached inline
// (Spec.Payload); a larger one is written to the artifact store first and
// referenced by Spec.PayloadRef. Exactly one of the two is ever set.
func storeScreenshotArtifact(ctx context.Context, deps screenshotStoreDeps, sess channelevents.SessionRef, png []byte) (*channelevents.AttachmentRef, error) {
	// Fail closed on a nil Client with an ERROR, not a nil-interface panic:
	// deps.Client is a plain client.Client interface field (Loop.Client), and a
	// Loop built without one (a kubectl-driven session, today's e2e in-process
	// harness — see Loop.screenshotStoreDeps' doc) must degrade to "ship no
	// screenshot" like any other capture/store failure, never crash the
	// dispatch goroutine calling it.
	if deps.Client == nil {
		return nil, fmt.Errorf("storeScreenshotArtifact: no k8s client wired (Loop.Client is nil)")
	}
	// Same fail-closed degrade for a missing Artifacts service: without it
	// awaitScreenshotReady cannot finalize the revision, and an AttachmentRef
	// minted without finalizing would point at an artifact nobody can ever
	// view — worse than shipping no screenshot at all, because it reads as a
	// broken image rather than a plain text-only card.
	if deps.Artifacts == nil {
		return nil, fmt.Errorf("storeScreenshotArtifact: no artifacts service wired (Loop.Artifacts is nil); the screenshot would never become viewable")
	}
	newRenderName := deps.NewRenderName
	if newRenderName == nil {
		newRenderName = artifacts.RenderName
	}
	newArtifactID := deps.NewArtifactID
	if newArtifactID == nil {
		newArtifactID = func() string { return memory.NewID(memartifact.Kind{}) }
	}

	artifactID := newArtifactID()
	renderName := newRenderName(sess.Name)

	spec := spiceboxv1alpha1.ArtifactRenderSpec{
		Kind:     channelevents.ScreenshotRendererKind,
		Filename: screenshotFilename,
		AltText:  screenshotAltText,
	}
	if len(png) <= screenshotInlineMaxBytes {
		spec.Payload = png
	} else {
		if deps.ArtifactStore == nil {
			return nil, fmt.Errorf("storeScreenshotArtifact: screenshot is %d bytes (> %d-byte inline cap) but no artifact store is wired", len(png), screenshotInlineMaxBytes)
		}
		key := fmt.Sprintf("%s/%s/%s/%s", sess.Namespace, sess.Name, artifactID, screenshotFilename)
		ref, err := deps.ArtifactStore.Put(ctx, key, bytes.NewReader(png))
		if err != nil {
			return nil, fmt.Errorf("storeScreenshotArtifact: store screenshot bytes: %w", err)
		}
		spec.PayloadRef = string(ref)
	}

	cr := artifacts.NewRender(renderName, sess.Namespace, sess.Name, deps.SessionUID, artifactID, spec, nil)
	if err := deps.Client.Create(ctx, cr); err != nil {
		return nil, fmt.Errorf("storeScreenshotArtifact: create ArtifactRender: %w", err)
	}

	if err := awaitScreenshotReady(ctx, deps, sess, cr); err != nil {
		return nil, fmt.Errorf("storeScreenshotArtifact: %w", err)
	}

	return &channelevents.AttachmentRef{
		RenderName: cr.Name,
		ArtifactID: artifactID,
		MIME:       channelevents.ScreenshotMIME,
		Filename:   screenshotFilename,
		AltText:    screenshotAltText,
	}, nil
}

// awaitScreenshotReady polls the just-created ArtifactRender CR until it
// reaches Ready or Failed, then finalizes the revision — the SAME
// CR-create+poll+finalize dance persistWidget (loop_uiresource.go) runs for a
// widget artifact. See storeScreenshotArtifact's doc comment for why this
// step is required rather than optional: FinalizeRevision is what wires the
// SpiceDB artifact#parent relationship and the revision_of memory link that
// pkg/web/webui/artifactview.bindView needs before it will serve a single
// byte of this screenshot back to the chat UI.
//
// The "image" renderer (pkg/channels/channelassets/image) runs in-process
// with no external I/O, so Ready lands within one reconcile tick in
// practice; screenshotRenderTimeout is a safety bound, not an expected wait.
func awaitScreenshotReady(ctx context.Context, deps screenshotStoreDeps, sess channelevents.SessionRef, cr *spiceboxv1alpha1.ArtifactRender) error {
	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	deadline := time.Now().Add(screenshotRenderTimeout)
	// lastErr retains the most recent non-NotFound Get failure across poll
	// iterations so the terminal timeout can report WHY it never saw Ready, rather
	// than swallowing a persistent transport/RBAC error behind a bare "timed out".
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		var fresh spiceboxv1alpha1.ArtifactRender
		if err := deps.Client.Get(ctx, client.ObjectKeyFromObject(cr), &fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("ArtifactRender vanished before completion")
			}
			// A transient Get fault (cache not synced, transport blip) is worth a
			// retry, but not worth losing: keep it so a timeout can surface it.
			lastErr = err
			time.Sleep(screenshotRenderPollInterval)
			continue
		}
		switch fresh.Status.Phase {
		case spiceboxv1alpha1.ArtifactRenderPhaseReady:
			// A system approval, minted here rather than inherited from ctx —
			// the same per-write-site idiom host_approval.go's writeCtx
			// (memory.WithSystemApproval(ctx, "tool_approval") /
			// "plan_gate") already uses for the runner's own trusted internal
			// writes. Needed because captureBrowserScreenshotFn's captureCtx is
			// deliberately rooted at context.Background() (decoupled from the
			// caller's ctx, see its doc), so no approval the caller's ctx might
			// have carried would reach here anyway; mirrors production, where
			// l.Artifacts wraps an HTTP memory client that enforces this
			// server-side on the bearer token, not from ctx at all — this call
			// only matters against an in-process memory.Local (every test, any
			// future in-process deployment).
			if _, err := deps.Artifacts.FinalizeRevision(memory.WithSystemApproval(ctx, "browser_screenshot"), scope, &fresh); err != nil {
				return fmt.Errorf("render Ready but recording the revision failed: %w", err)
			}
			return nil
		case spiceboxv1alpha1.ArtifactRenderPhaseFailed:
			return fmt.Errorf("render failed: %s — %s", fresh.Status.FailureReason, fresh.Status.FailureMessage)
		}
		time.Sleep(screenshotRenderPollInterval)
	}
	if lastErr != nil {
		return fmt.Errorf("timed out waiting for ArtifactRender to become Ready: last error: %w", lastErr)
	}
	return fmt.Errorf("timed out waiting for ArtifactRender to become Ready")
}

// screenshotStoreDeps builds storeScreenshotArtifact's backend wiring from
// this Loop's own fields: l.Client creates (and awaitScreenshotReady polls)
// the ArtifactRender CR, l.Artifacts finalizes it once Ready — the exact same
// pair applyUIResource's own CR create+poll+finalize uses (loop.go's
// uiResourceDeps{Client: l.Client, Artifacts: l.Artifacts, ...}), wired
// together for the same reason: either may be nil (a kubectl-driven session,
// today's e2e in-process harness), and storeScreenshotArtifact fails closed
// when so — and l.AgentSession's UID owns it (mirrors loop.go's own "var
// sessionUID types.UID; if l.AgentSession != nil { sessionUID =
// l.AgentSession.UID }" guard — empty when Loop.AgentSession is unset).
//
// ArtifactStore is deliberately left nil: Loop carries no write-capable
// artifactstore.Store today. l.ArtifactReader is read-only
// (modality.ArtifactReader) — not a screenshotArtifactPutter. That means only
// the <=screenshotInlineMaxBytes inline Payload path is reachable from this
// wiring; a screenshot over the cap makes storeScreenshotArtifact return an
// error, which the caller (captureBrowserScreenshotFn below) logs and treats
// as "ship no screenshot" — same as any other capture/store failure, per the
// best-effort/fail-closed contract. A single-viewport redacted PNG is well
// under 256 KiB in practice, so this degrades gracefully rather than
// requiring a new store seam to be threaded through Loop for this feature.
func (l *Loop) screenshotStoreDeps() screenshotStoreDeps {
	var sessionUID types.UID
	if l.AgentSession != nil {
		sessionUID = l.AgentSession.UID
	}
	return screenshotStoreDeps{
		Client:     l.Client,
		Artifacts:  l.Artifacts,
		SessionUID: sessionUID,
	}
}

// captureBrowserScreenshotFn builds the best-effort, fail-closed closure a
// runnerHost's captureScreenshot field carries: capture a redacted screenshot
// via cap, store it as an ArtifactRender through l, and return the
// AttachmentRef to attach to a browser interaction payload — or nil, having
// logged why, on ANY capture or store failure. A capture/store failure NEVER
// surfaces as an error to the caller; the approval publish always proceeds.
//
// Shared by BOTH places a browser-sourced ask gets published (D-R3: one
// browser session, one capturer, two publish call sites, because
// request_human_step is itself a browser tool): executeToolContained wires it
// onto the per-call host it builds for the web_host#act acting-approval
// (plan_phase ask) AND the web_action/web_host tool_call JIT approval, and
// humanStepRequester.Confirm/RequestCode wire it onto the host THEY bind for
// the login_human_step ask — different *runnerHost values (the latter is
// bound fresh per round-trip via hostBinder, see its doc), but the SAME Loop
// and the SAME capturer, which is all this closure needs.
//
// The returned closure's ctx argument is the CALLER's ask-raise/dispatch ctx
// — it is NOT used as the capture's deadline. Observed live on a credentialed
// desktop run: every approval card shipped text-only, logging "capture
// failed; publishing text-only" / "context deadline exceeded", because the
// capture inherited that ctx's deadline, which is far tighter than a cold
// first capture against a real browser pod needs. The capture is best-effort
// by contract (a failure here never reaches the caller, it only means the
// card ships without a picture), so it must not die to a deadline that exists
// to bound the TURN, not the screenshot.
//
// captureCtx below gets its OWN deadline (captureScreenshotTimeout), rooted at
// context.Background() rather than derived from ctx, so the two are
// decoupled. The caller's CANCELLATION (not its deadline) still propagates:
// context.AfterFunc fires on ctx.Done() but only calls cancel when
// ctx.Err() == context.Canceled — an explicit interrupt (the turn aborted,
// the session ended) still tears the capture down, but ctx's deadline simply
// elapsing (context.DeadlineExceeded) does not, which is the exact failure
// mode this fixes.
//
// Task #53: #47's decoupling helped (captures now sometimes succeed) but a
// live desktop run still logged "capture failed ... context deadline
// exceeded" intermittently — the failure is the kind a second attempt
// usually clears (a contended browser mutex, a CDP round trip that was
// merely slow, not stuck). So on a timeout-shaped failure (and ONLY then —
// see isTimeoutErr) this retries cap.CaptureRedacted ONCE, against the SAME
// captureCtx rather than a fresh one. Reusing captureCtx is what keeps the
// retry bounded: it gets whatever is left of the original
// captureScreenshotTimeout budget, never a second full budget, so the
// closure's worst-case wall time is unchanged — a card can never hang twice
// as long waiting for a picture. If captureCtx is already done (its budget
// fully spent, or the caller genuinely canceled), captureCtx.Err() != nil and
// the retry is skipped outright.
func (l *Loop) captureBrowserScreenshotFn(sess channelevents.SessionRef, cap browsertool.BrowserScreenshotCapturer) func(context.Context) *channelevents.AttachmentRef {
	return func(ctx context.Context) *channelevents.AttachmentRef {
		captureStart := time.Now()
		captureCtx, cancel := context.WithTimeout(context.Background(), captureScreenshotTimeout)
		defer cancel()
		stop := context.AfterFunc(ctx, func() {
			if ctx.Err() == context.Canceled {
				cancel()
			}
		})
		defer stop()

		attempts := 1
		png, err := cap.CaptureRedacted(captureCtx)
		if err != nil && isTimeoutErr(err) && captureCtx.Err() == nil {
			attempts = 2
			png, err = cap.CaptureRedacted(captureCtx)
		}
		if err != nil {
			slog.Info("browser screenshot: capture failed; publishing text-only",
				"session", sess.Namespace+"/"+sess.Name, "attempts", attempts,
				"elapsed_ms", time.Since(captureStart).Milliseconds(), "err", err.Error())
			return nil
		}
		ref, err := storeScreenshotArtifact(captureCtx, l.screenshotStoreDeps(), sess, png)
		if err != nil {
			slog.Info("browser screenshot: store failed; publishing text-only",
				"session", sess.Namespace+"/"+sess.Name, "attempts", attempts,
				"elapsed_ms", time.Since(captureStart).Milliseconds(), "err", err.Error())
			return nil
		}
		slog.Info("browser screenshot: capture succeeded",
			"session", sess.Namespace+"/"+sess.Name, "attempts", attempts,
			"elapsed_ms", time.Since(captureStart).Milliseconds())
		return ref
	}
}

// isTimeoutErr reports whether err is the deadline/cancellation-shaped
// failure captureBrowserScreenshotFn retries once: CaptureRedacted's own
// inner timeout elapsing naturally (context.DeadlineExceeded), or an
// explicit cancel forwarded into the chromedp run by session.go's do()
// (context.Canceled — which is what a late-firing deadline on captureCtx
// itself surfaces as, since do()'s propagation always calls the runCtx
// CancelFunc explicitly rather than letting runCtx's own timer fire). A
// non-timeout failure — notably CaptureRedacted's fail-closed redaction
// errors (a blanked field whose box genuinely can't be resolved) — is NOT
// retried: the retry targets the Task #53 symptom specifically, not every
// way the capture can fail.
func isTimeoutErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
