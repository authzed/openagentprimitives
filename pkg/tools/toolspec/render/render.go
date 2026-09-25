// Package render produces plain-language descriptions of ToolSpecs.
// Pure function: (spec, toolkit, format) → string. No network, no LLM.
//
// The pipeline is two-stage: buildReport (in report.go) projects a
// (spec, toolkit) pair into a *report — the structured intermediate
// shape used by all output formats — and the renderText / renderMarkdown
// functions in format_*.go (or json.MarshalIndent for the JSON format)
// turn that into bytes. Per-rule fallback descriptions live in
// templates.go.
package render

import (
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// Format selects an output format.
type Format string

const (
	FormatText     Format = "text"
	FormatMarkdown Format = "markdown"
	FormatJSON     Format = "json"
)

// Describe renders sp + tk in the chosen format.
func Describe(sp *spec.Spec, tk *toolkit.Toolkit, f Format) (string, error) {
	r := buildReport(sp, tk)
	switch f {
	case FormatText:
		return renderText(r), nil
	case FormatMarkdown:
		return renderMarkdown(r), nil
	case FormatJSON:
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("unknown format %q", f)
	}
}
