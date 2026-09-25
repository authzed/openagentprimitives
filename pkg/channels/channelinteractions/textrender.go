package channelinteractions

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// RenderText renders an interaction as markdown: the universal floor every
// output-capable channel kind can deliver. Rich kinds (Slack blocks, webchat
// cards, TUI prompts) override presentation; this guarantees no interaction is
// ever undeliverable.
//
// The inert-fence guarantee is CommonMark-specific. A kind whose display
// dialect is not CommonMark (Slack mrkdwn) must apply its own inert treatment
// rather than reusing this output verbatim.
//
// decisionURL maps a decision/link_mint action ID to its hosted decision-page
// URL. Nil renders those actions as plain labels plus a pointer to respond
// from a connected surface.
func RenderText(p channelevents.InteractionRequestPayload, decisionURL func(actionID string) string) string {
	var b strings.Builder

	// Tone marker, for notice categories only. The floor has no colour and no
	// glyphs, so tone has to be a word — which is also what a screen reader, an
	// email digest, and `oap` piped to a file all get right. A prompt is left
	// unmarked: its actions already say what it is.
	if cat, ok := Get(p.Category); ok && cat.Notice {
		if marker := textMarker(cat.Tone); marker != "" {
			fmt.Fprintf(&b, "%s ", marker)
		}
	}
	fmt.Fprintf(&b, "**%s**\n", p.Lead)
	if p.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", p.Body)
	}
	// NextStep is emphasised and set on its own line on every surface: it is
	// the one part of a notice a stuck user is looking for.
	if p.NextStep != "" {
		fmt.Fprintf(&b, "\n**%s**\n", p.NextStep)
	}
	if len(p.Fields) > 0 {
		b.WriteString("\n")
		for _, f := range p.Fields {
			fmt.Fprintf(&b, "- **%s:** %s\n", f.Label, f.Value)
		}
	}
	if p.Excerpt != nil {
		b.WriteString("\n")
		// Build inner text as label + content (label untrusted, must be inside fence)
		var inner strings.Builder
		if p.Excerpt.Label != "" {
			fmt.Fprintf(&inner, "%s:\n", p.Excerpt.Label)
		}
		inner.WriteString(p.Excerpt.Content)
		innerText := inner.String()
		fence := inertFence(innerText)
		fmt.Fprintf(&b, "%s\n%s\n%s\n", fence, innerText, fence)
	}

	if len(p.Actions) > 0 {
		b.WriteString("\n")
		unlinkable := false
		for _, a := range p.Actions {
			switch {
			case a.Kind == channelevents.ActionKindLink:
				fmt.Fprintf(&b, "- [%s](%s)\n", a.Label, a.URL)
			case decisionURL != nil:
				fmt.Fprintf(&b, "- [%s](%s)\n", a.Label, decisionURL(a.ID))
			default:
				fmt.Fprintf(&b, "- %s\n", a.Label)
				unlinkable = true
			}
		}
		if unlinkable {
			b.WriteString("\n_To act on this, respond from a connected app._\n")
		}
	}
	return b.String()
}

// inertFence returns a code fence strictly longer than any backtick run in
// content, so untrusted content can never escape its fence.
func inertFence(content string) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// textMarker is the tone word the text floor prefixes a notice's lead with.
// A bracketed word rather than an emoji: the floor must be the rendering that
// CANNOT fail, on surfaces with no colour, no emoji font, and no markup.
//
// Routine and housekeeping return "" — marking every ordinary thing just
// trains readers to skip the marker.
func textMarker(t Tone) string {
	switch t {
	case ToneCritical:
		return "[!]"
	case TonePrivacy:
		return "[privacy]"
	case ToneDegraded:
		return "[degraded]"
	case ToneWaiting:
		return "[waiting]"
	case ToneResolved:
		return "[ok]"
	default:
		return ""
	}
}
