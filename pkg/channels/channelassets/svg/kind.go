// Package svg is the SVG artifact renderer. SVG is a security-critical input:
// it is XML that can carry <script>, on* event handlers, <foreignObject> with
// arbitrary HTML inside, animation elements that rewrite an href to
// javascript: AFTER a naive value check (the GO-2026-4667 / CVE-2026-31807
// class of bypass), and <use>/<image>/xlink:href pointing at remote resources.
//
// This renderer VALIDATES rather than rewrites: a payload is checked against a
// hardened allowlist (github.com/hamochi/safesvg, whose default allowlist is
// derived from DOMPurify/cure53, tightened further in New). Safe payloads pass
// through byte-for-byte, because fidelity matters. Unsafe payloads do NOT
// hard-fail — they render a 1x1 blank placeholder carrying structured warnings
// naming what to remove. The only hard error from Render is input exceeding
// MaxInputSize.
//
// Delivery is BundledOnly: SVG output is never the top-level browser document;
// it is only ever shown inside an <img> (or composed into the html kind's
// preview), where the SVG document's own scripts never execute. The hardened
// allowlist is defense-in-depth on top of that containment, not a substitute.
package svg

import (
	"github.com/hamochi/safesvg"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

const (
	maxInput  = 256 << 10 // 256 KiB
	maxOutput = 1 << 20   // 1 MiB
)

// Renderer validates SVG against a hardened allowlist and passes safe bytes
// through unchanged. The validator is built once in New() and reused across
// calls (it is stateless after configuration).
type Renderer struct {
	validator safesvg.Validator
}

// New builds the hardened validator and returns a ready Renderer.
//
// Hardening on top of safesvg's default allowlist:
//
//   - Whitelist the camelCase presentation elements the dependency stores under
//     lowercase map keys. Go's encoding/xml is case-preserving, so without this
//     a legitimate <linearGradient> would be rejected. (The fe* filter elements
//     are already stored camelCase upstream and need no addition.)
//   - Blacklist dangerous elements. Some are in the default allowlist (use,
//     image, the animate* family), some are not (script, foreignObject, a,
//     iframe, audio, video, handler, listener, set); blacklisting all of them
//     regardless keeps the allowlist unambiguous and survives a dependency
//     change that adds one back. animate*/set matter specifically because they
//     can rewrite an href to javascript: after a value check.
//   - Blacklist href and xlink:href so no element can carry an external or
//     javascript: navigation/script target.
//
// XXE is not reachable: safesvg validates with encoding/xml, which does not
// resolve external entities — a <!ENTITY x SYSTEM "file:///..."> reference
// surfaces as an "invalid character entity" parse error, which we treat as a
// validation failure (blank + warn).
func New() *Renderer {
	v := safesvg.NewValidator()

	v.WhitelistElements(
		"linearGradient", "radialGradient", "clipPath", "textPath",
		"altGlyph", "altGlyphDef", "altGlyphItem", "glyphRef",
	)

	v.BlacklistElements(
		"script", "foreignObject", "foreignobject", "a", "use", "image",
		"iframe", "audio", "video", "handler", "listener",
		"animate", "animateColor", "animatecolor",
		"animateMotion", "animatemotion",
		"animateTransform", "animatetransform", "set",
	)

	v.BlacklistAttributes("href", "xlink:href")

	return &Renderer{validator: v}
}

func (Renderer) Kind() string { return "svg" }
func (Renderer) ExecutionMode() channelassets.ExecutionMode {
	return channelassets.ExecutionModeInProcess
}
func (Renderer) MaxInputSize() int64                  { return maxInput }
func (Renderer) MaxOutputSize() int64                 { return maxOutput }
func (Renderer) OutputMIMEs() []string                { return []string{"image/svg+xml"} }
func (Renderer) InputMIMEs() []string                 { return []string{"image/svg+xml"} }
func (Renderer) SupportsLiveView() bool               { return true }
func (Renderer) AgentSelectable() bool                { return true }
func (Renderer) Delivery() channelassets.DeliveryMode { return channelassets.DeliveryBundledOnly }

func (Renderer) Instructions() string {
	return "SVG is validated against a hardened safe allowlist (no rewriting — valid SVG is " +
		"delivered byte-for-byte). Disallowed: <script>, on* event handlers, <foreignObject>, " +
		"<a> navigation, the animation elements (<animate>, <animateMotion>, <animateTransform>, " +
		"<set>) that can rewrite a link to javascript:, and external references " +
		"(<use>, <image>, href, xlink:href). An SVG containing any of these renders as a blank " +
		"1x1 image with warnings naming exactly what to remove — read them, fix the source, and " +
		"re-render. SVG is only ever shown inside an <img>, so even if a script slipped through " +
		"it would not execute; the allowlist is defense-in-depth. Provide the SVG markup as the payload."
}

// ServeTransform is a no-op: the validated SVG is served as-is.
func (Renderer) ServeTransform(content []byte) []byte { return content }
