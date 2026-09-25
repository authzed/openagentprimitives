package css

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/internal/csssanitize"
)

// Render sanitizes the CSS payload and returns the cleaned bytes. Behavior:
//
//   - Safe payload   → Output.Bytes is byte-identical to the input; no Warnings.
//   - Unsafe payload → Output.Bytes has the banned constructs spliced out;
//     Warnings names each stripped construct. err is nil; delivery is never
//     blocked by content.
//   - Input over MaxInputSize → the ONE hard error (also respects ctx.Err()).
func (r *Renderer) Render(ctx context.Context, in channelassets.Input) (channelassets.Output, error) {
	if int64(len(in.Payload)) > maxInput {
		return channelassets.Output{}, fmt.Errorf("input %d bytes > max %d", len(in.Payload), maxInput)
	}
	if err := ctx.Err(); err != nil {
		return channelassets.Output{}, err
	}

	clean, warnings := csssanitize.Sanitize(string(in.Payload))
	return channelassets.Output{
		Bytes:    []byte(clean),
		MIME:     "text/css",
		Filename: ensureCSSExt(in.Filename),
		AltText:  in.AltText,
		Warnings: warnings,
	}, nil
}

// ensureCSSExt guarantees a .css filename. Mirrors the svg kind's ensureSVGExt.
func ensureCSSExt(name string) string {
	const ext = ".css"
	if name == "" {
		return "style" + ext
	}
	if strings.HasSuffix(strings.ToLower(name), ext) {
		return name
	}
	return name + ext
}
