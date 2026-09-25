// Package css is the CSS artifact renderer. It delivers standalone stylesheets
// as a bundled artifact (BundledOnly): the CSS is never the top-level browser
// document; it is only ever applied to a host HTML page. The renderer runs the
// same CSS denylist as the html kind's inline-style sanitizer, which is the
// second layer of defense after the host page's Content-Security-Policy.
package css

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

const (
	maxInput  = 256 << 10 // 256 KiB
	maxOutput = 1 << 20   // 1 MiB
)

// Renderer sanitizes standalone CSS stylesheets and delivers them as
// text/css. It is stateless; New() is cheap.
type Renderer struct{}

// New returns a ready Renderer.
func New() *Renderer { return &Renderer{} }

func (Renderer) Kind() string { return "css" }
func (Renderer) ExecutionMode() channelassets.ExecutionMode {
	return channelassets.ExecutionModeInProcess
}
func (Renderer) MaxInputSize() int64                  { return maxInput }
func (Renderer) MaxOutputSize() int64                 { return maxOutput }
func (Renderer) OutputMIMEs() []string                { return []string{"text/css"} }
func (Renderer) InputMIMEs() []string                 { return []string{"text/css"} }
func (Renderer) SupportsLiveView() bool               { return true }
func (Renderer) AgentSelectable() bool                { return true }
func (Renderer) Delivery() channelassets.DeliveryMode { return channelassets.DeliveryBundledOnly }

func (Renderer) Instructions() string {
	return "CSS is sanitized using the same denylist as the HTML kind's inline-style sanitizer. " +
		"The following constructs are removed and reported as warnings: " +
		"(1) @import — external stylesheet pulls are never allowed; inline all styles. " +
		"(2) url(...) whose target is not a data: URI or a #fragment — only url(data:...) and url(#frag) survive; " +
		"embed images and fonts as data: URIs. " +
		"(3) expression(...) — legacy IE CSS scripting, always removed. " +
		"(4) behavior / -moz-binding properties in declaration position — legacy script-binding, always removed. " +
		"Safe CSS (everything not in the denylist) is delivered byte-for-byte; the sanitizer does not reformat or rewrite. " +
		"Warnings name each stripped construct and its occurrence count so you can correct the source and re-render. " +
		"This is a bundled stylesheet: it is applied to a host HTML page, not served standalone. " +
		"Provide the raw CSS text as the payload (content type text/css)."
}

// ServeTransform is a no-op: the sanitized CSS is served as-is.
func (Renderer) ServeTransform(content []byte) []byte { return content }
