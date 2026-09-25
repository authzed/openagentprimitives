// Package slack — pure Block Kit rendering for the tool_session
// sub-channel. A tool run is shown as a 3-block message: a header
// naming the tool + the agent's intent, a status/elapsed context
// line, and the streamed output in a fixed-width code block.
package slack

import (
	"fmt"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"
)

const (
	// toolSessionTailLines bounds the code block to its last N lines. A
	// short block keeps the Slack message under Slack's "Show more"
	// auto-collapse threshold — collapse resets on every chat.update, so
	// a long block would re-hide its newest output behind the fold on
	// every edit. 12 lines is a comfortable always-visible live pane.
	toolSessionTailLines = 12
	// toolSessionTailMax is a byte backstop for a few pathologically
	// long lines (well under the Slack section-block 3000-char limit).
	toolSessionTailMax = 2000
)

// toolSessionView is the renderable state of one tool run.
type toolSessionView struct {
	toolName   string
	reason     string
	transcript string
	elapsed    time.Duration
	finished   bool
	ok         bool
	costUSD    float64
}

// formatElapsed renders a duration compactly: "45s", "1m23s", "1h02m".
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	}
}

// codeBlockTail returns the last toolSessionTailLines lines of s (a byte
// backstop also applies), with surrounding blank lines / whitespace
// trimmed so the rendered code block has no leading or trailing empty
// line. A marker is prefixed when earlier content was dropped. Slack
// shows only this live tail; the full transcript is persisted in the
// tool_session memory Kind.
func codeBlockTail(s string) string {
	lines := strings.Split(s, "\n")
	truncated := false
	if len(lines) > toolSessionTailLines {
		lines = lines[len(lines)-toolSessionTailLines:]
		truncated = true
	}
	out := strings.Join(lines, "\n")
	if len(out) > toolSessionTailMax {
		out = out[len(out)-toolSessionTailMax:]
		truncated = true
	}
	// Trim surrounding whitespace so the code block has no leading or
	// trailing blank line.
	out = strings.Trim(out, " \t\r\n")
	if truncated && out != "" {
		out = "…(earlier output truncated)\n" + out
	}
	return out
}

// renderToolSessionBlocks builds the 3-block Block Kit message.
func renderToolSessionBlocks(v toolSessionView) []slackapi.Block {
	emoji := "▶️"
	if v.finished {
		emoji = "✅"
		if !v.ok {
			emoji = "❌"
		}
	}

	// The header is a mrkdwn section, and both halves of it come from the wire.
	//
	// toolName is the dispatched OUTER tool the runner names (pl.OuterTool,
	// sender_tool_session.go), so a hostile value is not the live threat here —
	// but this renderer wraps it in a code span of its own, and a value carrying
	// a backtick closes that span early and renders the rest as live mrkdwn, so
	// it is made span-safe rather than trusted to stay well-formed.
	//
	// reason is the MODEL's own `_reason` for the dispatching tool call
	// (channelevents.ToolSessionEventPayload.Reason) and lands in free mrkdwn
	// with no delimiter around it — so it takes the full prose sweep. Unswept it
	// could open a `<!channel>` ping or a forged `<url|label>` action inside
	// platform chrome, which is the one thing a reader is entitled to read as
	// the platform speaking rather than the agent.
	//
	// The transcript below needs neither: its sink is a rich_text preformatted
	// element, which Slack renders literally.
	label := "tool session"
	if v.toolName != "" {
		label = "`" + inertSpanValue(v.toolName) + "`"
	}
	header := emoji + " " + label
	if v.reason != "" {
		header += " — " + inertProse(v.reason)
	}

	var status string
	switch {
	case !v.finished:
		status = "running · " + formatElapsed(v.elapsed)
	case v.costUSD > 0:
		word := "done"
		if !v.ok {
			word = "failed"
		}
		status = fmt.Sprintf("%s · $%.4f · %s", word, v.costUSD, formatElapsed(v.elapsed))
	default:
		word := "done"
		if !v.ok {
			word = "failed"
		}
		status = fmt.Sprintf("%s · %s", word, formatElapsed(v.elapsed))
	}

	body := codeBlockTail(v.transcript)
	if body == "" {
		body = "(no output yet)"
	}

	return []slackapi.Block{
		slackapi.NewSectionBlock(slackapi.NewTextBlockObject("mrkdwn", header, false, false), nil, nil),
		slackapi.NewContextBlock("", slackapi.NewTextBlockObject("mrkdwn", status, false, false)),
		// The output goes in a rich_text preformatted block tagged
		// "shell" — Slack renders that as a fixed-width code block
		// labelled "Shell". A plain ``` fence in a section block carries
		// no language, so it shows no label.
		slackapi.NewRichTextBlock("", &slackapi.RichTextPreformatted{
			Type:     slackapi.RTEPreformatted,
			Language: "shell",
			Elements: []slackapi.RichTextSectionElement{
				slackapi.NewRichTextSectionTextElement(body, nil),
			},
		}),
	}
}
