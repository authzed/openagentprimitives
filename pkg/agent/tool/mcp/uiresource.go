package mcp

import (
	"strings"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	probe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// isUIResource reports whether an embedded-resource content block is an MCP UI
// resource. Both mcp-ui and MCP Apps use the ui:// (or ui-app://) URI scheme;
// MCP Apps additionally marks the body text/html;profile=mcp-app. Matching the
// scheme is the reliable signal for both.
func isUIResource(b probe.ContentBlock) bool {
	if b.Type != "resource" {
		return false
	}
	if strings.HasPrefix(b.URI, "ui://") || strings.HasPrefix(b.URI, "ui-app://") {
		return true
	}
	return strings.Contains(b.ResourceMIMEType, "profile=mcp-app")
}

// uiResourceFrom extracts the widget document from a UI-resource content block.
// HTML comes from the resource's blob, else its text. Returns (nil, false) for a
// non-UI block or one with no document body (unusable — the caller falls back to
// the [resource: URI] placeholder).
func uiResourceFrom(b probe.ContentBlock, origin, tool string) (*agenttool.UIResourceSpec, bool) {
	if !isUIResource(b) {
		return nil, false
	}
	html := b.ResourceBlob
	if len(html) == 0 && b.ResourceText != "" {
		html = []byte(b.ResourceText)
	}
	if len(html) == 0 {
		return nil, false
	}
	return &agenttool.UIResourceSpec{
		URI:      b.URI,
		MIMEType: b.ResourceMIMEType,
		HTML:     html,
		Meta:     b.ResourceMeta,
		Origin:   origin,
		Tool:     tool,
	}, true
}
