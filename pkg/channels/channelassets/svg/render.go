package svg

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

// emptySVG is the inert placeholder returned when validation fails. It is a
// valid, harmless 1x1 SVG — the agent reads the accompanying warnings, fixes
// the source, and re-renders.
const emptySVG = `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"></svg>`

// Render validates the payload against the hardened allowlist. Behavior:
//
//   - Safe payload  → Output.Bytes is the ORIGINAL bytes, unchanged (we
//     validate, we do not rewrite — fidelity matters), MIME image/svg+xml.
//   - Unsafe payload → Output.Bytes is the empty placeholder and Warnings names
//     the validation issue. err is nil; delivery is never blocked by content.
//   - Input over MaxInputSize → the ONE hard error (also respects ctx.Err()).
func (r *Renderer) Render(ctx context.Context, in channelassets.Input) (channelassets.Output, error) {
	if int64(len(in.Payload)) > maxInput {
		return channelassets.Output{}, fmt.Errorf("input %d bytes > max %d", len(in.Payload), maxInput)
	}
	if err := ctx.Err(); err != nil {
		return channelassets.Output{}, err
	}

	if issue := r.validate(in.Payload); issue != "" {
		return channelassets.Output{
			Bytes:    []byte(emptySVG),
			MIME:     "image/svg+xml",
			Filename: ensureSVGExt(in.Filename),
			AltText:  in.AltText,
			Warnings: []channelassets.Warning{{
				Kind:   "svg",
				Name:   "validation",
				Action: "rejected",
				Count:  1,
				Note:   issue,
			}},
		}, nil
	}

	return channelassets.Output{
		Bytes:    in.Payload,
		MIME:     "image/svg+xml",
		Filename: ensureSVGExt(in.Filename),
		AltText:  in.AltText,
	}, nil
}

// validate returns "" when the payload is a safe SVG, or a human-readable
// description of the first violation otherwise. Two gates, both of which must
// pass:
//
//  1. The payload must have an <svg> root element. safesvg's allowlist check
//     only fires on disallowed StartElements, so a payload with NO recognized
//     elements at all ("not svg at all", which the XML decoder sees as bare
//     character data) passes its allowlist vacuously. Requiring a real <svg>
//     root closes that hole — non-SVG input is treated as unsafe.
//  2. The hardened safesvg allowlist must accept every element and attribute.
func (r *Renderer) validate(payload []byte) string {
	if !hasSVGRoot(payload) {
		return "not a recognizable SVG document (missing <svg> root element)"
	}
	if err := r.validator.Validate(payload); err != nil {
		return err.Error()
	}
	return ""
}

// hasSVGRoot reports whether payload is well-formed XML whose first element is
// <svg>. It uses the same encoding/xml decoder safesvg uses, so the two gates
// share parsing semantics (and the same XXE-safe behavior: external entities
// are never resolved).
func hasSVGRoot(payload []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(payload))
	for {
		tok, err := dec.Token()
		if err != nil {
			// EOF before any element, or a parse error (e.g. an XXE entity
			// reference) → not a usable SVG.
			if err == io.EOF {
				return false
			}
			return false
		}
		if se, ok := tok.(xml.StartElement); ok {
			return strings.EqualFold(se.Name.Local, "svg")
		}
	}
}

// ensureSVGExt guarantees a .svg filename. Mirrors the image kind's ensureExt.
func ensureSVGExt(name string) string {
	const ext = ".svg"
	if name == "" {
		return "image" + ext
	}
	if strings.HasSuffix(strings.ToLower(name), ext) {
		return name
	}
	return name + ext
}
