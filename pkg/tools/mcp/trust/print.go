// Package trust renders SEP-1913 trust annotation snapshots in a
// compact human-readable form. Used by both the gen-time authoring
// prompt (the LLM-facing per-tool summary) and the operator-facing
// `oap tools mcp check` output.
//
// See https://github.com/modelcontextprotocol/modelcontextprotocol/pull/1913
package trust

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// FromProbe renders SEP-1913 trust hints from a probed Annotations
// block. Returns "" when nothing was asserted.
func FromProbe(a mcpprobe.Annotations) string {
	return renderLine(a.MaliciousActivityHint, a.Attribution, a.InputMetadata, a.ReturnMetadata)
}

// FromSpec renders the same shape from a persisted spec.Trust block.
// Used by `oap tools mcp check` to show the operator what got recorded.
func FromSpec(t mcpspec.Trust) string {
	return renderLine(t.MaliciousActivityHint, t.Attribution, t.InputMetadata, t.ReturnMetadata)
}

// renderLine is the shared formatting body.
func renderLine(mal bool, attribution []string, inputMeta, returnMeta json.RawMessage) string {
	parts := []string{}
	if mal {
		parts = append(parts, "maliciousActivityHint=true")
	}
	if len(attribution) > 0 {
		parts = append(parts, fmt.Sprintf("attribution=%v", attribution))
	}
	if hasContent(inputMeta) {
		parts = append(parts, "inputMetadata=<see tool JSON>")
	}
	if hasContent(returnMeta) {
		parts = append(parts, "returnMetadata=<see tool JSON>")
	}
	return strings.Join(parts, ", ")
}

// hasContent returns true when raw is a non-empty, non-null JSON
// value that's not an empty object.
func hasContent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return false
	}
	return true
}
