// Package render produces plain-language descriptions of one MCP tool's
// effective contract. Pure function: (spec, toolName, format) -> string.
// No network, no LLM. Mirrors pkg/tools/toolspec/render: buildReport projects
// the spec into a *report; renderText / renderMarkdown / JSON turn that
// into bytes. Raw CEL is never emitted — only Constraint.Message.
package render

import (
	"encoding/json"
	"fmt"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// Format selects an output format.
type Format string

const (
	FormatText     Format = "text"
	FormatMarkdown Format = "markdown"
	FormatJSON     Format = "json"
)

// Describe renders the named tool from sp in the chosen format.
// Returns an error if toolName is not present in sp.Tools.
func Describe(sp *mcpspec.Spec, toolName string, f Format) (string, error) {
	r, err := buildReport(sp, toolName)
	if err != nil {
		return "", err
	}
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
