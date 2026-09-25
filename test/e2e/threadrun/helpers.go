//go:build e2e

package threadrun

import (
	"bytes"
	"io"
	"maps"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// lastUserText returns the most recent user text block, walking backward past
// tool-result-only messages — the same traversal the harness's own user-text
// matcher performs, so an expectation reads the way a rule would.
func lastUserText(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		var b strings.Builder
		for _, c := range m.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		if s := b.String(); strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// lastToolResultName returns the tool whose result is the latest message, or ""
// when the latest message is not a tool result.
//
// Tool results carry the tool_use ID rather than the name, so the name is
// recovered by finding the assistant tool_use that ID answers. Matching on the
// ID alone would make a transcript unreadable.
func lastToolResultName(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		for _, c := range req.Messages[i].Content {
			if c.Type != "tool_result" {
				continue
			}
			if c.ToolResult == nil {
				return ""
			}
			return toolNameForUseID(req, c.ToolResult.ToolUseID)
		}
	}
	return ""
}

// lastToolResultBlock returns the most recent tool_result block, or nil when
// the request carries none.
func lastToolResultBlock(req llm.Request) *llm.ToolResultBlock {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		for _, c := range req.Messages[i].Content {
			if c.Type == "tool_result" {
				return c.ToolResult
			}
		}
	}
	return nil
}

// latestToolResultCounts summarizes the most recent message containing tool
// results. A single assistant reply can emit several tool uses that dispatch
// concurrently, so their result positions do not express execution order.
func latestToolResultCounts(req llm.Request) map[string]bt.ToolResultCount {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		counts := map[string]bt.ToolResultCount{}
		found := false
		for _, c := range req.Messages[i].Content {
			if c.Type != "tool_result" || c.ToolResult == nil {
				continue
			}
			found = true
			name := toolNameForUseID(req, c.ToolResult.ToolUseID)
			count := counts[name]
			count.Total++
			if c.ToolResult.IsError {
				count.Errors++
			}
			counts[name] = count
		}
		if found {
			return counts
		}
	}
	return nil
}

func toolResultCountsMatch(req llm.Request, want map[string]bt.ToolResultCount) bool {
	return maps.Equal(want, latestToolResultCounts(req))
}

func toolNameForUseID(req llm.Request, useID string) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		for _, c := range req.Messages[i].Content {
			if c.Type == "tool_use" && c.ToolUse != nil && c.ToolUse.ID == useID {
				return c.ToolUse.Name
			}
		}
	}
	return ""
}

func hasTool(req llm.Request, name string) bool {
	for _, t := range req.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
