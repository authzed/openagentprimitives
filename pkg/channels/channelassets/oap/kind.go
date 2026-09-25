// Package oap is the renderer for a packaged agent — the .oap bundle a
// workshop exports as its draft. It is OPERATOR-ONLY: an agent cannot select
// it, only the workshop draft route (pkg/web/workshopdraftsrv) creates a
// render of it, and the bytes it renders are the same bytes the admin install
// path reads, digest-bound. Render verifies the bytes open as a bundle and
// passes them through unchanged: there is nothing to sanitize in a container
// of manifests the install path validates again on its own, and rewriting
// them would break the digest the install request carries.
package oap

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	oapfmt "github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// maxBytes is the operator's absolute render cap. A draft above it fails its
// render as PayloadTooLarge, which artifact_await reports to the builder; the
// install path, which reads the draft by its own reference, is unaffected.
const maxBytes = 10 << 20

// Renderer is the bundle pass-through. Stateless.
type Renderer struct{}

func New() *Renderer { return &Renderer{} }

func (Renderer) Kind() string { return "oap" }
func (Renderer) ExecutionMode() channelassets.ExecutionMode {
	return channelassets.ExecutionModeInProcess
}
func (Renderer) Delivery() channelassets.DeliveryMode { return channelassets.DeliveryStandalone }
func (Renderer) MaxInputSize() int64                  { return maxBytes }
func (Renderer) MaxOutputSize() int64                 { return maxBytes }
func (Renderer) OutputMIMEs() []string                { return []string{oapfmt.ArtifactType} }
func (Renderer) InputMIMEs() []string                 { return []string{oapfmt.ArtifactType} }
func (Renderer) SupportsLiveView() bool               { return false }
func (Renderer) AgentSelectable() bool                { return false }
func (Renderer) Instructions() string                 { return "" }

// ServeTransform is a no-op: the bundle is never framed, only downloaded.
func (Renderer) ServeTransform(content []byte) []byte { return content }

// Render refuses anything that is not a bundle and otherwise returns the bytes
// unchanged, under the bundle MIME and a filename ending in the bundle
// extension.
func (Renderer) Render(_ context.Context, in channelassets.Input) (channelassets.Output, error) {
	if _, err := oapfmt.Unpack(in.Payload); err != nil {
		return channelassets.Output{}, fmt.Errorf("%w: not an agent bundle: %v", channelassets.ErrMalformedPayload, err)
	}
	name := in.Filename
	if name == "" {
		name = "draft"
	}
	if !strings.HasSuffix(name, oapfmt.DefaultExtension) {
		name += oapfmt.DefaultExtension
	}
	return channelassets.Output{Bytes: in.Payload, MIME: oapfmt.ArtifactType, Filename: name, AltText: in.AltText}, nil
}
