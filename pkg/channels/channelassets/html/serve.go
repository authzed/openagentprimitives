package html

// ServeTransform is a no-op: the sanitized artifact HTML is served inert.
// Under the live-view's nested-frame isolation the artifact is the INNER,
// script-disabled frame (sandbox="allow-same-origin", no allow-scripts) of a
// host page that owns all live-view behavior — scroll preservation, revision
// swaps — through same-origin DOM access. Nothing is injected and the baked
// meta-CSP is served unchanged; default-src 'none' forbids scripts, and the
// structural no-allow-scripts frame is the real guarantee.
func (Renderer) ServeTransform(content []byte) []byte { return content }
