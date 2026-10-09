package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
)

// Slack's documented Block Kit limits for the presentational blocks this
// channel accepts from an agent. Enforcing them in the runner — before the
// envelope is published — turns a would-be post-time rejection (which degrades
// to plain text) into a precise, correctable error the model sees on the
// respond_to_user call.
const (
	maxBlocksPerMessage  = 50
	maxSectionTextLen    = 3000
	maxHeaderTextLen     = 150
	maxContextElements   = 10
	maxSectionFields     = 10
	maxFieldTextLen      = 2000
	maxMarkdownTextLen   = 12000
	maxDataVizTitleLen   = 50
	maxContainerChildren = 10
)

// componentFormattingInstructions is the model-facing authoring guide for the
// `blocks` field, injected into respond_to_user the same way the mrkdwn dialect
// instructions are injected into `text`. It names the allowlist, the limits,
// and the prohibitions so the model stays inside what ValidateComponents
// accepts.
const componentFormattingInstructions = `This channel can render structured Block Kit layout. ` +
	`To use it, pass a ` + "`blocks`" + ` array of Slack Block Kit blocks alongside your text. ` +
	`Supported (read-only) blocks: ` +
	`"header" (plain_text, ≤150 chars); ` +
	`"section" (a mrkdwn "text" object and/or up to 10 "fields"; no accessory); ` +
	`"divider"; ` +
	`"context" (text/mrkdwn elements, ≤10); ` +
	`"rich_text" (sections, bullet/ordered lists, quotes, preformatted/code); ` +
	`"markdown" (a standard-markdown block, ≤12000 chars); ` +
	`"table" (rows of rich_text cells); ` +
	`"data_visualization" (a chart; REQUIRES a "title" ≤50 chars and a "chart" object. ` +
	`pie: {"type":"pie","segments":[{"label":"Organic","value":60}]}. ` +
	`bar/area/line: {"type":"bar","series":[{"name":"Signups","data":[{"label":"Mon","value":10}]}],"axis_config":{"categories":["Mon","Tue"]}} — note each series uses "data" (not "data_points") and axis_config.categories is required; ≤2 charts per message); ` +
	`"plan" and "task_card" (task lists); ` +
	`"card" (title/subtitle/body/subtext — NO action buttons, NO images yet); ` +
	`"container" (groups 1–10 read-only child blocks in "child_blocks"; REQUIRES a "title" or "rich_text_title"). ` +
	`Section/context text uses the same Slack mrkdwn dialect as the text field (*bold*, _italic_, ` + "`code`" + `, <url|label>) — not CommonMark; the "markdown" block is the exception and takes standard markdown. ` +
	`Limits: at most 50 blocks; section text ≤3000 characters; split longer content across multiple blocks. ` +
	`Not supported: interactive elements (buttons, select menus, date pickers, inputs, action/input/context_actions blocks, data_table, carousel) — this surface is presentation-only; ` +
	`images and icons (coming soon); and @channel/@here/@everyone broadcast mentions. ` +
	`Always also fill the text field with a concise plain summary: it is the notification preview and the fallback shown if the blocks cannot be rendered.`

// ComponentFormattingInstructions implements channelkinds.ComponentFormatter.
func (*Kind) ComponentFormattingInstructions() string { return componentFormattingInstructions }

// ValidateComponents implements channelkinds.ComponentValidator.
func (*Kind) ValidateComponents(raw json.RawMessage) error { return validateComponents(raw) }

// ComponentsPlainText implements channelkinds.ComponentValidator. It projects
// the Block Kit payload to the same readable text the inbound path derives from
// blocks (flattenBlocks), so the info-leakage egress gate measures what the
// blocks say. Best-effort: unparseable input yields "" — ValidateComponents,
// not this, is the authority on rejecting bad input.
func (*Kind) ComponentsPlainText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ""
	}
	var blocks slackapi.Blocks
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return ""
	}
	return flattenBlocks(blocks.BlockSet)
}

// validateComponents checks agent-authored Block Kit JSON against the
// presentation-only contract: an allowlist of read-only layout blocks, Slack's
// structural limits, no broadcast mentions, and — for now — no images.
// Interactive blocks and elements are rejected so the feature never mints a
// click that would have to route back into the session. The returned error is
// model-facing: it names the offending block and says how to fix it.
func validateComponents(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New("respond_to_user: `blocks` must be a non-empty JSON array of Block Kit blocks")
	}
	if trimmed[0] != '[' {
		return errors.New("respond_to_user: `blocks` must be a JSON array of Block Kit blocks, e.g. [{\"type\":\"section\",\"text\":{\"type\":\"mrkdwn\",\"text\":\"…\"}}]")
	}

	var blocks slackapi.Blocks
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return fmt.Errorf("respond_to_user: `blocks` could not be parsed as Block Kit JSON: %v", err)
	}
	set := blocks.BlockSet
	if len(set) == 0 {
		return errors.New("respond_to_user: `blocks` must contain at least one block")
	}
	if len(set) > maxBlocksPerMessage {
		return fmt.Errorf("respond_to_user: too many blocks (%d); Slack renders at most %d blocks per message", len(set), maxBlocksPerMessage)
	}

	for i, b := range set {
		if err := validateBlock(fmt.Sprintf("block %d", i), b); err != nil {
			return err
		}
	}
	return nil
}

func blockErrf(loc, typ, format string, args ...any) error {
	return fmt.Errorf("respond_to_user: %s (%s): "+format, append([]any{loc, typ}, args...)...)
}

// validateBlock validates one block (loc is a human path like "block 2" or
// "block 2 child 1"). Read-only layout blocks pass; interactive, image, and
// unknown blocks are rejected with a model-facing reason.
func validateBlock(loc string, b slackapi.Block) error {
	switch blk := b.(type) {
	case *slackapi.HeaderBlock:
		if blk.Text != nil {
			if n := len(blk.Text.Text); n > maxHeaderTextLen {
				return blockErrf(loc, "header", "text is %d characters; Slack limits header text to %d", n, maxHeaderTextLen)
			}
			if err := checkBroadcast(loc, "header", blk.Text.Text); err != nil {
				return err
			}
		}
		return nil
	case *slackapi.SectionBlock:
		return validateSection(loc, blk)
	case *slackapi.DividerBlock:
		return nil
	case *slackapi.ContextBlock:
		return validateContext(loc, blk)
	case *slackapi.RichTextBlock:
		return validateRichText(loc, blk)
	case *slackapi.MarkdownBlock:
		if n := len(blk.Text); n > maxMarkdownTextLen {
			return blockErrf(loc, "markdown", "text is %d characters; Slack limits a markdown block to %d", n, maxMarkdownTextLen)
		}
		return checkBroadcast(loc, "markdown", blk.Text)
	case *slackapi.TableBlock:
		return validateTable(loc, blk)
	case *slackapi.DataVisualizationBlock:
		return validateDataViz(loc, blk)
	case *slackapi.CardBlock:
		return validateCard(loc, blk)
	case *slackapi.ContainerBlock:
		return validateContainer(loc, blk)
	case *slackapi.PlanBlock:
		if err := checkBroadcast(loc, "plan", blk.Title); err != nil {
			return err
		}
		for j := range blk.Tasks {
			if err := validateTaskCard(fmt.Sprintf("%s task %d", loc, j), &blk.Tasks[j]); err != nil {
				return err
			}
		}
		return nil
	case *slackapi.TaskCardBlock:
		return validateTaskCard(loc, blk)
	case *slackapi.AlertBlock:
		return blockErrf(loc, "alert", "alert blocks are only valid in modals, not in messages; use a section (with *bold* and an emoji) or a markdown block for an inline callout")
	case *slackapi.ImageBlock:
		return blockErrf(loc, "image", "image blocks are not yet supported on this channel; omit them for now")
	case *slackapi.ActionBlock, *slackapi.InputBlock, *slackapi.ContextActionsBlock:
		return blockErrf(loc, string(b.BlockType()), "interactive components (buttons, menus, inputs) are not supported; this channel accepts presentation-only layout")
	case *slackapi.DataTableBlock:
		return blockErrf(loc, "data_table", "the data_table block is interactive; use a read-only \"table\" block instead")
	case *slackapi.CarouselBlock:
		return blockErrf(loc, "carousel", "the carousel block is interactive (its cards carry actions); it is not supported on this channel")
	default:
		return blockErrf(loc, string(b.BlockType()), "unsupported block type; allowed: header, section, divider, context, rich_text, markdown, table, data_visualization, plan, task_card, card, container")
	}
}

func validateSection(loc string, s *slackapi.SectionBlock) error {
	// A section accessory is either an image (slice 2) or an interactive element
	// (never): reject any accessory rather than enumerate the ways it's unsupported.
	if s.Accessory != nil {
		return blockErrf(loc, "section", "accessories are not supported on this channel; use plain section text and fields")
	}
	if s.Text != nil {
		if n := len(s.Text.Text); n > maxSectionTextLen {
			return blockErrf(loc, "section", "text is %d characters; Slack limits section text to %d — split it across multiple section blocks", n, maxSectionTextLen)
		}
		if err := checkBroadcast(loc, "section", s.Text.Text); err != nil {
			return err
		}
	}
	if len(s.Fields) > maxSectionFields {
		return blockErrf(loc, "section", "has %d fields; Slack limits a section to %d fields", len(s.Fields), maxSectionFields)
	}
	for _, f := range s.Fields {
		if f == nil {
			continue
		}
		if n := len(f.Text); n > maxFieldTextLen {
			return blockErrf(loc, "section", "a field is %d characters; Slack limits each field to %d", n, maxFieldTextLen)
		}
		if err := checkBroadcast(loc, "section", f.Text); err != nil {
			return err
		}
	}
	return nil
}

func validateContext(loc string, c *slackapi.ContextBlock) error {
	els := c.ContextElements.Elements
	if len(els) > maxContextElements {
		return blockErrf(loc, "context", "has %d elements; Slack limits a context block to %d elements", len(els), maxContextElements)
	}
	for _, el := range els {
		text, ok := el.(*slackapi.TextBlockObject)
		if !ok {
			// The only non-text context element is an image, deferred to slice 2.
			return blockErrf(loc, "context", "image elements are not yet supported on this channel; use text-only context")
		}
		if err := checkBroadcast(loc, "context", text.Text); err != nil {
			return err
		}
	}
	return nil
}

func validateRichText(loc string, rt *slackapi.RichTextBlock) error {
	if richTextElementsHaveBroadcast(rt.Elements) {
		return blockErrf(loc, "rich_text", "broadcast mentions (@channel/@here/@everyone) are not allowed in agent-authored components")
	}
	return nil
}

func richTextElementsHaveBroadcast(els []slackapi.RichTextElement) bool {
	for _, el := range els {
		if richTextHasBroadcast(el) {
			return true
		}
	}
	return false
}

func validateTable(loc string, t *slackapi.TableBlock) error {
	for r, row := range t.Rows {
		for c, cell := range row {
			cloc := fmt.Sprintf("%s table r%dc%d", loc, r, c)
			switch cc := cell.(type) {
			case nil:
				// empty cell (Slack sends null) — nothing to validate.
			case *slackapi.TableRichTextCell:
				if richTextElementsHaveBroadcast(cc.Elements) {
					return blockErrf(cloc, "table", "broadcast mentions (@channel/@here/@everyone) are not allowed in agent-authored components")
				}
			case *slackapi.TableRawTextCell:
				if err := checkBroadcast(cloc, "table", cc.Text); err != nil {
					return err
				}
			case *slackapi.TableRawNumberCell:
				return blockErrf(cloc, "table", "raw_number cells are rejected by chat.postMessage; use a raw_text cell for a number in a message")
			default:
				return blockErrf(cloc, "table", "unsupported table cell type")
			}
		}
	}
	return nil
}

// validateDataViz enforces the data_visualization schema Slack's chat.postMessage
// requires (verified empirically against the live API): a non-empty title ≤50
// chars, a chart of a known type, and the per-type data shape — pie needs
// segments, bar/area/line need series plus axis_config.categories. Enforcing it
// here turns a would-be post-time invalid_blocks (which degrades the whole reply
// to plain text) into a precise error the agent can correct.
func validateDataViz(loc string, dv *slackapi.DataVisualizationBlock) error {
	if strings.TrimSpace(dv.Title) == "" {
		return blockErrf(loc, "data_visualization", "requires a non-empty \"title\" (≤%d chars)", maxDataVizTitleLen)
	}
	if n := len([]rune(dv.Title)); n > maxDataVizTitleLen {
		return blockErrf(loc, "data_visualization", "title is %d characters; Slack limits it to %d", n, maxDataVizTitleLen)
	}
	if err := checkBroadcast(loc, "data_visualization", dv.Title); err != nil {
		return err
	}
	switch c := dv.Chart.(type) {
	case *slackapi.DataVisualizationPieChart:
		if len(c.Segments) == 0 {
			return blockErrf(loc, "data_visualization", "pie chart requires a non-empty \"segments\" array of {label,value}")
		}
	case *slackapi.DataVisualizationBarChart:
		return validateVizSeries(loc, "bar", c.Series, c.AxisConfig)
	case *slackapi.DataVisualizationAreaChart:
		return validateVizSeries(loc, "area", c.Series, c.AxisConfig)
	case *slackapi.DataVisualizationLineChart:
		return validateVizSeries(loc, "line", c.Series, c.AxisConfig)
	default:
		return blockErrf(loc, "data_visualization", "requires a \"chart\" object whose \"type\" is one of pie, bar, area, line")
	}
	return nil
}

func validateVizSeries(loc, kind string, series []slackapi.DataVisualizationDataSeries, axis slackapi.DataVisualizationAxisConfig) error {
	if len(series) == 0 {
		return blockErrf(loc, "data_visualization", "%s chart requires a non-empty \"series\" array; each series is {\"name\":…,\"data\":[{\"label\":…,\"value\":…}]}", kind)
	}
	if len(axis.Categories) == 0 {
		return blockErrf(loc, "data_visualization", "%s chart requires \"axis_config\":{\"categories\":[…]} naming the x-axis categories", kind)
	}
	return nil
}

func validateContainer(loc string, c *slackapi.ContainerBlock) error {
	// Slack rejects a titleless container: "must define either `title`, or
	// `rich_text_title`" (verified against the live API).
	if c.Title == nil && c.RichTextTitle == nil {
		return blockErrf(loc, "container", "requires a \"title\" (plain_text/mrkdwn) or a \"rich_text_title\"")
	}
	if c.Icon != nil {
		return blockErrf(loc, "container", "container icons are not yet supported on this channel; omit them for now")
	}
	if c.Title != nil {
		if err := checkBroadcast(loc, "container", c.Title.Text); err != nil {
			return err
		}
	}
	if c.Subtitle != nil {
		if err := checkBroadcast(loc, "container", c.Subtitle.Text); err != nil {
			return err
		}
	}
	if c.RichTextTitle != nil {
		if err := validateRichText(loc, c.RichTextTitle); err != nil {
			return err
		}
	}
	n := len(c.ChildBlocks.BlockSet)
	if n == 0 {
		return blockErrf(loc, "container", "requires at least one block in \"child_blocks\"")
	}
	if n > maxContainerChildren {
		return blockErrf(loc, "container", "has %d child blocks; Slack limits a container to %d", n, maxContainerChildren)
	}
	for j, child := range c.ChildBlocks.BlockSet {
		if err := validateBlock(fmt.Sprintf("%s child %d", loc, j), child); err != nil {
			return err
		}
	}
	return nil
}

func validateCard(loc string, c *slackapi.CardBlock) error {
	if c.Actions != nil {
		return blockErrf(loc, "card", "card action buttons are interactive and not supported; use a card with only title/subtitle/body/subtext")
	}
	if c.HeroImage != nil || c.Icon != nil || c.SlackIcon != nil {
		return blockErrf(loc, "card", "card images and icons are not yet supported on this channel; omit them for now")
	}
	for _, t := range []*slackapi.TextBlockObject{c.Title, c.Subtitle, c.Body, c.Subtext} {
		if t == nil {
			continue
		}
		if err := checkBroadcast(loc, "card", t.Text); err != nil {
			return err
		}
	}
	return nil
}

func validateTaskCard(loc string, tc *slackapi.TaskCardBlock) error {
	if err := checkBroadcast(loc, "task_card", tc.Title); err != nil {
		return err
	}
	if tc.Details != nil {
		if err := validateRichText(loc, tc.Details); err != nil {
			return err
		}
	}
	if tc.Output != nil {
		if err := validateRichText(loc, tc.Output); err != nil {
			return err
		}
	}
	for _, s := range tc.Sources {
		if err := checkBroadcast(loc, "task_card", s.Text); err != nil {
			return err
		}
	}
	return nil
}

// richTextHasBroadcast walks the rich-text element tree looking for a broadcast
// element (the rich_text encoding of @channel/@here/@everyone).
func richTextHasBroadcast(el slackapi.RichTextElement) bool {
	switch e := el.(type) {
	case *slackapi.RichTextSection:
		return sectionElemsHaveBroadcast(e.Elements)
	case *slackapi.RichTextQuote:
		return sectionElemsHaveBroadcast(e.Elements)
	case *slackapi.RichTextPreformatted:
		return sectionElemsHaveBroadcast(e.Elements)
	case *slackapi.RichTextList:
		for _, sub := range e.Elements {
			if richTextHasBroadcast(sub) {
				return true
			}
		}
	}
	return false
}

func sectionElemsHaveBroadcast(els []slackapi.RichTextSectionElement) bool {
	for _, el := range els {
		if _, ok := el.(*slackapi.RichTextSectionBroadcastElement); ok {
			return true
		}
	}
	return false
}

func checkBroadcast(loc, typ, text string) error {
	if containsBroadcastMention(text) {
		return blockErrf(loc, typ, "broadcast mentions (@channel/@here/@everyone) are not allowed in agent-authored components")
	}
	return nil
}

// containsBroadcastMention reports whether text carries a Slack broadcast span.
// Slack writes these as <!channel>, <!here>, or <!everyone> (optionally with a
// |label), so matching the opening token catches every form.
func containsBroadcastMention(text string) bool {
	for _, tok := range []string{"<!channel", "<!here", "<!everyone"} {
		if strings.Contains(text, tok) {
			return true
		}
	}
	return false
}
