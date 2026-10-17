package channelevents

// ScreenshotRendererKind and ScreenshotMIME name the ArtifactRenderSpec.Kind
// and MIME type every redacted browser-screenshot AttachmentRef (see the
// Screenshot field on InteractionRequestPayload) is built from —
// pkg/agent/runner/browser_screenshot.go is the sole producer.
//
// Named here rather than spelled as literals in pkg/agent/runner: that
// package is under a structural guard
// (pkg/agent/runner/mime_opinion_guard_test.go) against hardcoded
// image/MIME string literals, because such a literal is normally a SECOND
// OPINION about what a model accepts that pkg/agent/llm/models/models.go
// should own exclusively. This is not that — a screenshot attachment's MIME
// is a channel-delivery concern, never carried to the model — so the fix is
// not a models.go row, it is exactly the guard's own documented escape valve:
// a named constant in the package that owns the concern, referenced by name.
// channelevents owns it because it already defines AttachmentRef and the
// Screenshot field that carries these values.
const (
	// ScreenshotRendererKind matches the registered Kind() of the raster-image
	// renderer (pkg/channels/channelassets/image).
	ScreenshotRendererKind = "image"
	// ScreenshotMIME is the MIME type of the redacted page screenshot PNG.
	ScreenshotMIME = "image/png"
)
