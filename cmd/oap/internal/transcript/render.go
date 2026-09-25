// Package transcript renders a session's stored transcript for the terminal:
// the turns `oap agent run` streams as they land, and the same turns plus
// interleaved tool-session events `oap session logs` replays afterwards. One
// package so a turn reads identically whichever command printed it.
package transcript

import (
	"fmt"
	"io"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolsession"
)

// RenderTurn renders one transcript turn: the speaker, its index, an optional
// token-usage badge, and each content block — text, tool_use, tool_result.
func RenderTurn(out io.Writer, th *tui.Theme, t memory.Turn) {
	var roleStyle string
	switch t.Role {
	case "user":
		roleStyle = th.Render(th.RoleUser, "user")
	case "assistant":
		roleStyle = th.Render(th.RoleAssistant, "assistant")
	case "system_note":
		roleStyle = th.Render(th.RoleSystem, "system_note")
	default:
		roleStyle = t.Role
	}
	usage := ""
	if t.Usage != nil {
		usage = fmt.Sprintf(" %s", th.Render(th.Badge, fmt.Sprintf("in=%d out=%d", t.Usage.InputTokens, t.Usage.OutputTokens)))
	}
	fmt.Fprintf(out, "\n%s (turn %d)%s:\n", roleStyle, t.Index, usage)
	for _, b := range t.Content {
		switch b.Type {
		case "text":
			fmt.Fprintln(out, "  "+strings.ReplaceAll(b.Text, "\n", "\n  "))
		case "tool_use":
			if b.ToolUse != nil {
				fmt.Fprintf(out, "  → %s %s(%s)\n", th.Render(th.Subtle, "tool_use:"), b.ToolUse.Name, string(b.ToolUse.Input))
			}
		case "tool_result":
			if b.ToolResult != nil {
				prefix := "  ← tool_result:"
				if b.ToolResult.IsError {
					prefix = "  ← " + th.Render(th.Err, "tool_result(error):")
				}
				snippet := b.ToolResult.Content
				if len(snippet) > 500 {
					snippet = snippet[:500] + "...(truncated)"
				}
				fmt.Fprintf(out, "%s %s\n", prefix, snippet)
			}
		}
	}
}

// RenderToolSessionEntry renders one tool_session memory Entry — a
// parsed event from an interactive tool's stream — as a single line,
// visually distinct from transcript turns.
func RenderToolSessionEntry(out io.Writer, th *tui.Theme, e memory.Entry) {
	ev, err := toolsession.EntryToEvent(e)
	if err != nil {
		fmt.Fprintf(out, "  %s %v\n", th.Render(th.Err, "tool-session decode error:"), err)
		return
	}
	label := th.Render(th.RoleTool, "tool")
	dim := func(s string) string { return th.Render(th.Subtle, s) }
	switch ev.EventType {
	case "tool_use_start":
		fmt.Fprintf(out, "%s  %s %s %s\n", label, dim("→"), ev.ToolName, dim(ev.Summary))
	case "tool_use_stop":
		mark := th.Render(th.Success, "✓")
		if !ev.OK {
			mark = th.Render(th.Err, "✗")
		}
		fmt.Fprintf(out, "%s  %s %s %s\n", label, mark, ev.ToolName, dim(ev.Summary))
	case "result":
		fmt.Fprintf(out, "%s  %s\n", label,
			dim(fmt.Sprintf("done ok=%t %dms $%.4f", ev.OK, ev.DurationMs, ev.CostUSD)))
	case "text_delta":
		fmt.Fprintln(out, label+"  "+strings.ReplaceAll(ev.Text, "\n", "\n        "))
	default:
		fmt.Fprintf(out, "%s  %s\n", label, dim(ev.EventType))
	}
}
