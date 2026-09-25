package svg

import (
	"context"
	"encoding/base64"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

// Compile-time assertion: Renderer satisfies the PreviewComposer interface.
var _ channelassets.PreviewComposer = Renderer{}

// PreviewHTML wraps the validated SVG as a data: URI inside <img> — the html
// kind allows data:image/svg+xml in <img src>, and an <img>-rendered SVG runs
// no script regardless of content (the safety boundary). gen is unused (the svg
// preview is deterministic).
func (Renderer) PreviewHTML(_ context.Context, content []byte, _ channelassets.MarkupGenerator) ([]byte, error) {
	b64 := base64.StdEncoding.EncodeToString(content)
	doc := `<!doctype html><html><head><title>SVG preview</title></head>` +
		`<body style="margin:0;display:flex;justify-content:center;align-items:center;min-height:100vh">` +
		`<img alt="SVG artifact" style="max-width:100%;height:auto" src="data:image/svg+xml;base64,` + b64 + `"></body></html>`
	return []byte(doc), nil
}
