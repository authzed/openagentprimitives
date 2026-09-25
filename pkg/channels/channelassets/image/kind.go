// Package image is the raster image renderer (PNG/JPEG/GIF). It sanitizes by
// decoding to pixels and re-encoding: the output is freshly serialized from
// the pixel grid, which strips ALL metadata (EXIF/ICC/comments) and defeats
// polyglots. WebP is unsupported because there is no pure-Go encoder.
package image

import "github.com/authzed/openagentprimitives/pkg/channels/channelassets"

const (
	maxInput     = 8 << 20 // 8 MiB
	maxOutput    = 8 << 20 // 8 MiB
	maxDimension = 4096
	jpegQuality  = 90
)

// Renderer is the raster image renderer. Stateless.
type Renderer struct{}

func New() *Renderer { return &Renderer{} }

func (Renderer) Kind() string { return "image" }
func (Renderer) ExecutionMode() channelassets.ExecutionMode {
	return channelassets.ExecutionModeInProcess
}
func (Renderer) MaxInputSize() int64                  { return maxInput }
func (Renderer) MaxOutputSize() int64                 { return maxOutput }
func (Renderer) OutputMIMEs() []string                { return []string{"image/png", "image/jpeg", "image/gif"} }
func (Renderer) InputMIMEs() []string                 { return []string{"image/png", "image/jpeg", "image/gif"} }
func (Renderer) SupportsLiveView() bool               { return false }
func (Renderer) AgentSelectable() bool                { return true }
func (Renderer) Delivery() channelassets.DeliveryMode { return channelassets.DeliveryStandalone }

func (Renderer) Instructions() string {
	return "Images are re-encoded from pixels (all metadata stripped). Accepted: PNG, JPEG, GIF; " +
		"capped at 4096px per side; GIF animation is preserved. Provide the image bytes as base64."
}

// ServeTransform is a no-op: a raster image is inert and served as-is.
func (Renderer) ServeTransform(content []byte) []byte { return content }
