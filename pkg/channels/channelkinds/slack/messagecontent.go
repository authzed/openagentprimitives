package slack

import (
	"strings"

	slackapi "github.com/slack-go/slack"
)

// messageText renders the human-readable content of a Slack message for
// transcript use.
//
// A message's `text` is NOT the whole story: Block Kit messages and messages
// built from the older `attachments` format routinely carry an EMPTY top-level
// text and put every readable field in `blocks` / `attachments`. Alerting
// webhooks (Alertmanager, Grafana, CI bots) post exactly that shape, so reading
// only m.Text drops the entire payload — the agent that was invited into an
// alert thread received a blank line where the alert should have been.
//
// The three sources are concatenated rather than treated as fallbacks: a
// message may legitimately carry a short top-level text AND an attachment body,
// and the agent should see both.
func messageText(m slackapi.Message) string {
	var parts []string
	text := strings.TrimSpace(m.Text)
	blocks := flattenBlocks(m.Blocks.BlockSet)

	// Slack auto-populates `text` with its OWN flattened rendering of `blocks`
	// (newlines collapsed to spaces), so a message carrying both usually says
	// the same thing twice — emitting both would roughly double every bot reply
	// in the transcript. When one subsumes the other, keep only the richer
	// rendering; when they genuinely differ, keep both.
	if text != "" && blocks != "" {
		nt, nb := normalizeWS(text), normalizeWS(blocks)
		switch {
		case strings.Contains(nb, nt):
			text = "" // blocks preserve the structure the fallback flattened away
		case strings.Contains(nt, nb):
			blocks = ""
		}
	}

	if text != "" {
		parts = append(parts, text)
	}
	if blocks != "" {
		parts = append(parts, blocks)
	}
	for _, att := range m.Attachments {
		if a := flattenAttachment(att); a != "" {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, "\n")
}

// normalizeWS collapses every run of whitespace to a single space so two
// renderings of the same content compare equal regardless of line breaks.
func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// authorLabel returns a display label for an app-authored message, or "" for a
// human-authored one (whose name is resolved via users.info instead).
//
// Bot messages carry no `user` id, so without this the transcript formatter
// falls all the way through to the literal string "unknown". Slack populates
// at most one of username / bot_profile.name; a webhook-posted alert may carry
// neither, in which case the bot id is still better attribution than nothing.
func authorLabel(m slackapi.Message) string {
	if m.Username != "" {
		return m.Username
	}
	if m.BotProfile != nil && m.BotProfile.Name != "" {
		return m.BotProfile.Name
	}
	return m.BotID
}

// flattenAttachment renders a legacy attachment as text. Field titles are kept
// alongside their values because alert attachments carry meaning in the label
// ("Severity: Warning" is not the same fact as a bare "Warning").
func flattenAttachment(att slackapi.Attachment) string {
	var parts []string
	for _, s := range []string{att.Pretext, att.Title, att.Text} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	for _, f := range att.Fields {
		title, value := strings.TrimSpace(f.Title), strings.TrimSpace(f.Value)
		switch {
		case title != "" && value != "":
			parts = append(parts, title+": "+value)
		case value != "":
			parts = append(parts, value)
		case title != "":
			parts = append(parts, title)
		}
	}
	// Fallback is the sender's own plain-text rendering of the attachment. Use
	// it only when nothing structured was extracted, so it never duplicates the
	// fields above.
	if len(parts) == 0 {
		return strings.TrimSpace(att.Fallback)
	}
	return strings.Join(parts, "\n")
}

// flattenBlocks renders a Block Kit block set as text, keeping only the blocks
// that carry readable content. Unknown/unhandled block types (images, dividers,
// actions) contribute nothing rather than erroring — a transcript is
// best-effort, and a new block type must never drop the blocks around it.
func flattenBlocks(blocks []slackapi.Block) string {
	var parts []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	for _, b := range blocks {
		switch blk := b.(type) {
		case *slackapi.SectionBlock:
			if blk.Text != nil {
				add(blk.Text.Text)
			}
			for _, f := range blk.Fields {
				if f != nil {
					add(f.Text)
				}
			}
		case *slackapi.HeaderBlock:
			if blk.Text != nil {
				add(blk.Text.Text)
			}
		case *slackapi.ContextBlock:
			for _, el := range blk.ContextElements.Elements {
				if t, ok := el.(*slackapi.TextBlockObject); ok {
					add(t.Text)
				}
			}
		case *slackapi.RichTextBlock:
			add(flattenRichText(blk.Elements))
		case *slackapi.MarkdownBlock:
			add(blk.Text)
		case *slackapi.TableBlock:
			// One line per row, cells " | "-separated, so a table read back
			// (or degraded to plain text) keeps its row structure.
			for _, row := range blk.Rows {
				var cells []string
				for _, cell := range row {
					if c := strings.TrimSpace(flattenTableCell(cell)); c != "" {
						cells = append(cells, c)
					}
				}
				add(strings.Join(cells, " | "))
			}
		case *slackapi.DataVisualizationBlock:
			add(flattenDataViz(blk))
		case *slackapi.CardBlock:
			for _, t := range []*slackapi.TextBlockObject{blk.Title, blk.Subtitle, blk.Body, blk.Subtext} {
				if t != nil {
					add(t.Text)
				}
			}
		case *slackapi.ContainerBlock:
			if blk.Title != nil {
				add(blk.Title.Text)
			}
			if blk.Subtitle != nil {
				add(blk.Subtitle.Text)
			}
			if blk.RichTextTitle != nil {
				add(flattenRichText(blk.RichTextTitle.Elements))
			}
			add(flattenBlocks(blk.ChildBlocks.BlockSet))
		case *slackapi.PlanBlock:
			add(blk.Title)
			for i := range blk.Tasks {
				add(flattenTaskCard(&blk.Tasks[i]))
			}
		case *slackapi.TaskCardBlock:
			add(flattenTaskCard(blk))
		}
	}
	return strings.Join(parts, "\n")
}

// flattenDataViz projects a chart's free-form agent-authored strings — the
// title plus every segment/data-point label, series name, and axis category/
// label. Projecting only the title (as this case once did) let an agent, or an
// injected prompt, route blockable content into chart labels and slip it past
// the egress leakage gate, which measures ComponentsPlainText -> flattenBlocks.
// Numeric values carry no text and are skipped. The strings come from
// vizLabels, the same walk the broadcast guard uses.
func flattenDataViz(dv *slackapi.DataVisualizationBlock) string {
	var parts []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	add(dv.Title)
	// A missing or unrecognized chart has no labels to project; validation
	// rejects it before anything is sent.
	labels, _ := vizLabels(dv.Chart)
	for _, l := range labels {
		add(l.value)
	}
	return strings.Join(parts, "\n")
}

// flattenTableCell renders a table cell's readable text. raw_number cells carry
// no text unless a display override is set; rich_text and raw_text cells carry
// their content.
func flattenTableCell(cell slackapi.TableCell) string {
	switch c := cell.(type) {
	case *slackapi.TableRichTextCell:
		return flattenRichText(c.Elements)
	case *slackapi.TableRawTextCell:
		return c.Text
	case *slackapi.TableRawNumberCell:
		return c.Text
	}
	return ""
}

// flattenTaskCard renders a task card's readable text (title, details, output,
// and source labels) for transcript/egress projection.
func flattenTaskCard(tc *slackapi.TaskCardBlock) string {
	var parts []string
	if tc.Title != "" {
		parts = append(parts, tc.Title)
	}
	if tc.Details != nil {
		parts = append(parts, flattenRichText(tc.Details.Elements))
	}
	if tc.Output != nil {
		parts = append(parts, flattenRichText(tc.Output.Elements))
	}
	// A source's URL renders as a clickable link, so it is content the egress
	// gate must measure, not just its label.
	for _, s := range tc.Sources {
		if src := linkText(s.URL, s.Text); src != "" {
			parts = append(parts, src)
		}
	}
	return strings.Join(parts, "\n")
}

// flattenRichText walks the rich_text element tree, concatenating its leaf text.
func flattenRichText(elements []slackapi.RichTextElement) string {
	var b strings.Builder
	for _, el := range elements {
		switch e := el.(type) {
		case *slackapi.RichTextSection:
			b.WriteString(flattenRichTextSection(e.Elements))
			b.WriteString("\n")
		case *slackapi.RichTextQuote:
			b.WriteString(flattenRichTextSection(e.Elements))
			b.WriteString("\n")
		case *slackapi.RichTextPreformatted:
			b.WriteString(flattenRichTextSection(e.Elements))
			b.WriteString("\n")
		case *slackapi.RichTextList:
			b.WriteString(flattenRichText(e.Elements))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// linkText renders a link in Slack's own mrkdwn form, <url|label> — the same
// text a section carrying that link holds. The URL is always kept: it is what
// the reader clicks, so projecting only the label would let a URL (and anything
// smuggled into its query string) slip past the egress gate unmeasured.
func linkText(url, label string) string {
	switch {
	case url == "":
		return label
	case label == "" || label == url:
		return "<" + url + ">"
	default:
		return "<" + url + "|" + label + ">"
	}
}

// flattenRichTextSection concatenates the leaf elements of one rich-text
// section. User/channel mentions keep their "<@Uxxx>" form so the transcript
// matches how the same text appears everywhere else in the pipeline.
func flattenRichTextSection(elements []slackapi.RichTextSectionElement) string {
	var b strings.Builder
	for _, el := range elements {
		switch e := el.(type) {
		case *slackapi.RichTextSectionTextElement:
			b.WriteString(e.Text)
		case *slackapi.RichTextSectionLinkElement:
			b.WriteString(linkText(e.URL, e.Text))
		case *slackapi.RichTextSectionUserElement:
			b.WriteString("<@" + e.UserID + ">")
		case *slackapi.RichTextSectionChannelElement:
			b.WriteString("<#" + e.ChannelID + ">")
		case *slackapi.RichTextSectionEmojiElement:
			b.WriteString(":" + e.Name + ":")
		}
	}
	return b.String()
}
