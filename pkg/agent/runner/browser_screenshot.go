package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"

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

	NewRenderName func(session string) string
	NewArtifactID func() string
}

// storeScreenshotArtifact persists a redacted page screenshot (the PNG bytes
// Task 2 produces) as an ArtifactRender{Kind:"image"} CR and returns the
// AttachmentRef the interaction request payload's Screenshot field carries.
//
// Mirrors the CR-build shape in pkg/agent/tool/meta/artifact_prepare.go:264-274
// and pkg/platform/artifacts.NewRender: owned by the session (by UID, as
// controller, blocking deletion), labeled with the artifact id. Unlike
// artifact_prepare / persistWidget (loop_uiresource.go) this does NOT poll the
// CR to Ready: a screenshot attaches to a live interaction request, not a
// model-facing handle, and AttachmentRef's own contract (see the doc comment
// on channelevents.AttachmentRef) is that the consumer re-Gets the CR and
// reads status.outputRef at delivery time — there is nothing for this call to
// wait on.
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

	return &channelevents.AttachmentRef{
		RenderName: cr.Name,
		ArtifactID: artifactID,
		MIME:       channelevents.ScreenshotMIME,
		Filename:   screenshotFilename,
		AltText:    screenshotAltText,
	}, nil
}

// screenshotStoreDeps builds storeScreenshotArtifact's backend wiring from
// this Loop's own fields: l.Client creates the ArtifactRender CR — the same
// value applyUIResource's own CR create+poll uses (loop.go's
// uiResourceDeps{Client: l.Client, ...}) — and l.AgentSession's UID owns it
// (mirrors loop.go's own "var sessionUID types.UID; if l.AgentSession != nil
// { sessionUID = l.AgentSession.UID }" guard — empty when Loop.AgentSession is
// unset, e.g. a kubectl-driven session or a unit test).
//
// ArtifactStore is deliberately left nil: Loop carries no write-capable
// artifactstore.Store today. l.Artifacts (*artifacts.Service) manages
// ArtifactRender CR headers/revisions over memory.Memory, and l.ArtifactReader
// is read-only (modality.ArtifactReader) — neither is a screenshotArtifactPutter.
// That means only the <=screenshotInlineMaxBytes inline Payload path is
// reachable from this wiring; a screenshot over the cap makes
// storeScreenshotArtifact return an error, which the caller
// (captureBrowserScreenshotFn below) logs and treats as "ship no screenshot" —
// same as any other capture/store failure, per the best-effort/fail-closed
// contract. A single-viewport redacted PNG is well under 256 KiB in practice,
// so this degrades gracefully rather than requiring a new store seam to be
// threaded through Loop for this feature.
func (l *Loop) screenshotStoreDeps() screenshotStoreDeps {
	var sessionUID types.UID
	if l.AgentSession != nil {
		sessionUID = l.AgentSession.UID
	}
	return screenshotStoreDeps{
		Client:     l.Client,
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
// (plan_phase ask), and humanStepRequester.Confirm/RequestCode wire it onto
// the host THEY bind for the login_human_step ask — two different *runnerHost
// values (the latter is bound fresh per round-trip via hostBinder, see its
// doc), but the SAME Loop and the SAME capturer, which is all this closure
// needs.
func (l *Loop) captureBrowserScreenshotFn(sess channelevents.SessionRef, cap browsertool.BrowserScreenshotCapturer) func(context.Context) *channelevents.AttachmentRef {
	return func(ctx context.Context) *channelevents.AttachmentRef {
		png, err := cap.CaptureRedacted(ctx)
		if err != nil {
			slog.Info("browser screenshot: capture failed; publishing text-only",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
			return nil
		}
		ref, err := storeScreenshotArtifact(ctx, l.screenshotStoreDeps(), sess, png)
		if err != nil {
			slog.Info("browser screenshot: store failed; publishing text-only",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
			return nil
		}
		return ref
	}
}
