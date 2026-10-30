package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"
)

// Slack's documented Block Kit limits for the presentational blocks this
// channel accepts from an agent. Enforcing them in the runner — before the
// envelope is published — turns a would-be post-time rejection (which degrades
// to plain text) into a precise, correctable error the model sees on the
// respond_to_user call. Every text limit is in characters (runes), as Slack
// counts them — a byte count would reject non-Latin text well under the limit.
// The per-message block and section caps are the sender's own constants
// (sender_chunk.go), so validation and rendering share one source of truth.
const (
	// maxAgentBlocks reserves one of Slack's 50 block slots for the
	// sender-appended mention_only "@-mention me to reply" footer, which a
	// mention_only thread cannot do without (a bare reply there never routes).
	// The validator has no routing-mode input, so it reserves the slot always.
	maxAgentBlocks         = slackMaxBlocksPerMessage - 1
	maxSectionTextLen      = slackSectionTextLimit
	maxHeaderTextLen       = 150
	maxContextElements     = 10
	maxSectionFields       = 10
	maxFieldTextLen        = 2000
	maxMarkdownTextLen     = 12000
	maxDataVizTitleLen     = 50
	maxDataVizAxisLabelLen = 50
	maxContainerChildren   = 10
	maxChartsPerMessage    = 2
	maxTablesPerMessage    = 1
	maxTableRows           = 100
	maxTableColumns        = 20
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
	`"table" (rows of rich_text cells; one table per message, ≤100 rows, ≤20 columns); ` +
	`"data_visualization" (a chart; REQUIRES a "title" ≤50 chars and a "chart" object. ` +
	`pie: {"type":"pie","segments":[{"label":"Organic","value":60}]} — 1–6 segments. ` +
	`bar/area/line: {"type":"bar","series":[{"name":"Signups","data":[{"label":"Mon","value":10}]}],"axis_config":{"categories":["Mon","Tue"]}} — each series uses "data" (not "data_points") and axis_config.categories is required. ` +
	`Slack caps every chart: 1–6 series; 1–20 x-axis categories, all distinct; EXACTLY one data point per category per series, and each point's "label" must be the identical string as its category (not a reformatted date); bucket long time ranges into ≤20 points — do not emit a point per minute; all category labels, series names, and point labels 1–20 chars and not blank; axis_config "x_label"/"y_label" are optional, ≤50 chars; pie segment values must be > 0 (omit zero buckets); ≤2 charts per message); ` +
	`"plan" and "task_card" (task lists); ` +
	`"card" (title/subtitle/body/subtext — NO action buttons, NO images yet); ` +
	`"container" (groups 1–10 read-only child blocks in "child_blocks"; REQUIRES a "title" or "rich_text_title"). ` +
	`Section/context text uses the same Slack mrkdwn dialect as the text field (*bold*, _italic_, ` + "`code`" + `, <url|label>) — not CommonMark; the "markdown" block is the exception and takes standard markdown. ` +
	`Limits: at most 49 blocks; section text ≤3000 characters; split longer content across multiple blocks. ` +
	`Every block needs content: no empty header, section, context, markdown, or table. ` +
	`Not supported: interactive elements (buttons, select menus, date pickers, inputs, action/input/context_actions blocks, data_table, carousel) — this surface is presentation-only; ` +
	`images and icons (coming soon); and @channel/@here/@everyone broadcast mentions plus <!subteam^…> usergroup pings. ` +
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
	if len(set) > maxAgentBlocks {
		return fmt.Errorf("respond_to_user: too many blocks (%d); at most %d blocks per message (Slack's %d, less one reserved for a system footer)", len(set), maxAgentBlocks, slackMaxBlocksPerMessage)
	}

	for i, b := range set {
		if err := validateBlock(fmt.Sprintf("block %d", i), b); err != nil {
			return err
		}
	}

	// Per-message counts Slack enforces across the whole message, containers
	// included: checking only top-level blocks would let a container smuggle a
	// third chart into a post Slack then rejects whole.
	var charts, tables int
	countBlocks(set, &charts, &tables)
	if charts > maxChartsPerMessage {
		return fmt.Errorf("respond_to_user: too many data_visualization blocks (%d); Slack renders at most %d charts per message — send the rest in another reply", charts, maxChartsPerMessage)
	}
	if tables > maxTablesPerMessage {
		return fmt.Errorf("respond_to_user: too many table blocks (%d); Slack allows %d table per message — send the rest in another reply", tables, maxTablesPerMessage)
	}
	return nil
}

// countBlocks tallies chart and table blocks, descending into containers.
func countBlocks(set []slackapi.Block, charts, tables *int) {
	for _, b := range set {
		switch blk := b.(type) {
		case *slackapi.DataVisualizationBlock:
			*charts++
		case *slackapi.TableBlock:
			*tables++
		case *slackapi.ContainerBlock:
			countBlocks(blk.ChildBlocks.BlockSet, charts, tables)
		}
	}
}

// runeLen counts characters the way Slack's limits do.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

func blockErrf(loc, typ, format string, args ...any) error {
	return fmt.Errorf("respond_to_user: %s (%s): "+format, append([]any{loc, typ}, args...)...)
}

// validateBlock validates one block (loc is a human path like "block 2" or
// "block 2 child 1"). Read-only layout blocks pass; interactive, image, and
// unknown blocks are rejected with a model-facing reason.
func validateBlock(loc string, b slackapi.Block) error {
	switch blk := b.(type) {
	case *slackapi.HeaderBlock:
		if blk.Text == nil || strings.TrimSpace(blk.Text.Text) == "" {
			return blockErrf(loc, "header", "requires non-empty \"text\" ({\"type\":\"plain_text\",\"text\":…})")
		}
		if n := runeLen(blk.Text.Text); n > maxHeaderTextLen {
			return blockErrf(loc, "header", "text is %d characters; Slack limits header text to %d", n, maxHeaderTextLen)
		}
		return checkBroadcast(loc, "header", blk.Text.Text)
	case *slackapi.SectionBlock:
		return validateSection(loc, blk)
	case *slackapi.DividerBlock:
		return nil
	case *slackapi.ContextBlock:
		return validateContext(loc, blk)
	case *slackapi.RichTextBlock:
		return validateRichText(loc, blk)
	case *slackapi.MarkdownBlock:
		if strings.TrimSpace(blk.Text) == "" {
			return blockErrf(loc, "markdown", "requires non-empty \"text\"")
		}
		if n := runeLen(blk.Text); n > maxMarkdownTextLen {
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
	hasText := s.Text != nil && strings.TrimSpace(s.Text.Text) != ""
	if !hasText && len(s.Fields) == 0 {
		return blockErrf(loc, "section", "requires a non-empty \"text\" object or at least one entry in \"fields\"")
	}
	if s.Text != nil {
		if n := runeLen(s.Text.Text); n > maxSectionTextLen {
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
		if n := runeLen(f.Text); n > maxFieldTextLen {
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
	if len(els) == 0 {
		return blockErrf(loc, "context", "requires at least one text element in \"elements\"")
	}
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
		return blockErrf(loc, "rich_text", "broadcast or usergroup mentions (@channel, @here, @everyone, <!subteam^…>) are not allowed in agent-authored components")
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
	if len(t.Rows) == 0 {
		return blockErrf(loc, "table", "requires at least one row in \"rows\"")
	}
	if len(t.Rows) > maxTableRows {
		return blockErrf(loc, "table", "has %d rows; Slack limits a table to %d rows — split it or summarize", len(t.Rows), maxTableRows)
	}
	for r, row := range t.Rows {
		if len(row) > maxTableColumns {
			return blockErrf(loc, "table", "row %d has %d cells; Slack limits a table to %d columns", r, len(row), maxTableColumns)
		}
		for c, cell := range row {
			cloc := fmt.Sprintf("%s table r%dc%d", loc, r, c)
			switch cc := cell.(type) {
			case nil:
				// empty cell (Slack sends null) — nothing to validate.
			case *slackapi.TableRichTextCell:
				if richTextElementsHaveBroadcast(cc.Elements) {
					return blockErrf(cloc, "table", "broadcast or usergroup mentions (@channel, @here, @everyone, <!subteam^…>) are not allowed in agent-authored components")
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
// requires by delegating to slack-go's DataVisualizationBlock.Validate, which
// models Slack's per-chart rules (verified against the live API): 1–6 series or
// pie segments, 1–20 unique categories, exactly one point per category whose
// label matches it, 1–20-char labels, pie values > 0, and the title/axis-label
// caps. Enforcing it here turns a would-be post-time invalid_blocks (which
// degrades the whole reply to plain text, sender.go) into a located error the
// agent can correct.
//
// The checks run in an order chosen for the model reading the error:
//
//  1. The no-mass-notification guard over EVERY free-form chart string, the
//     title plus segment labels, series names, data-point labels, axis
//     categories, and axis labels. Those strings render in Slack, so checking
//     only the title would leave a mass-ping path open. It runs before every
//     length check so an over-long <!subteam^…> or <!channel> string is told
//     to drop the ping, not to shorten the string and still carry it.
//  2. Title and axis-label lengths, locally. Validate would catch them too,
//     but those errors must not carry dataVizLimitsHint, whose series and
//     label rules say nothing about a 60-char y_label.
//  3. Whitespace-only labels, which Validate's length check lets through.
//  4. Validate, whose remaining failures are all series/category/label rules,
//     so each one carries the full rule set.
func validateDataViz(loc string, dv *slackapi.DataVisualizationBlock) error {
	if strings.TrimSpace(dv.Title) == "" {
		return blockErrf(loc, "data_visualization", "requires a non-empty \"title\" (≤%d chars)", maxDataVizTitleLen)
	}
	labels, ok := vizLabels(dv.Chart)
	if !ok {
		return blockErrf(loc, "data_visualization", "requires a \"chart\" object whose \"type\" is one of pie, bar, area, line")
	}
	if err := checkBroadcast(loc, "data_visualization", dv.Title); err != nil {
		return fmt.Errorf("%w (in title)", err)
	}
	for _, l := range labels {
		if err := checkBroadcast(loc, "data_visualization", l.value); err != nil {
			return fmt.Errorf("%w (in %s)", err, l.path)
		}
	}
	if n := runeLen(dv.Title); n > maxDataVizTitleLen {
		return blockErrf(loc, "data_visualization", "\"title\" is %d chars; the limit is %d", n, maxDataVizTitleLen)
	}
	for _, l := range labels {
		if n := runeLen(l.value); l.axis && n > maxDataVizAxisLabelLen {
			return blockErrf(loc, "data_visualization", "%s is %d chars; the limit is %d", l.path, n, maxDataVizAxisLabelLen)
		}
	}
	for _, l := range labels {
		if !l.axis && l.value != "" && strings.TrimSpace(l.value) == "" {
			return blockErrf(loc, "data_visualization", "%s is whitespace-only; every category, series name, and label needs visible text", l.path)
		}
	}
	if err := dv.Validate(); err != nil {
		return blockErrf(loc, "data_visualization", "%v. %s", err, dataVizLimitsHint)
	}
	return nil
}

// dataVizLimitsHint is appended to a Validate failure so the model gets the
// whole rule set, not just the first rule its chart broke.
const dataVizLimitsHint = `Slack chart rules: 1–6 series (pie: 1–6 segments, each value > 0); ` +
	`1–20 unique axis_config.categories (bucket long time ranges into ≤20 points); ` +
	`each series has exactly one point per category, and each point's label must equal its category string; ` +
	`every category, series name, and label is 1–20 characters`

// vizLabel is one free-form chart string, located for error messages. axis
// marks the optional x_label/y_label, which may be empty and have their own cap.
type vizLabel struct {
	path  string
	value string
	axis  bool
}

// vizLabels collects a chart's free-form strings, below the title. ok is false
// for a missing or unrecognized chart. It is the one walk over chart strings:
// the broadcast guard here and the leakage-gate projection (flattenDataViz)
// both read it, so a field added to a chart reaches both or neither.
func vizLabels(chart slackapi.DataVisualizationChart) (labels []vizLabel, ok bool) {
	switch c := chart.(type) {
	case *slackapi.DataVisualizationPieChart:
		if c == nil {
			return nil, false
		}
		for i, seg := range c.Segments {
			labels = append(labels, vizLabel{path: fmt.Sprintf("segments[%d].label", i), value: seg.Label})
		}
		return labels, true
	case *slackapi.DataVisualizationBarChart:
		if c == nil {
			return nil, false
		}
		return vizSeriesLabels(c.Series, c.AxisConfig), true
	case *slackapi.DataVisualizationAreaChart:
		if c == nil {
			return nil, false
		}
		return vizSeriesLabels(c.Series, c.AxisConfig), true
	case *slackapi.DataVisualizationLineChart:
		if c == nil {
			return nil, false
		}
		return vizSeriesLabels(c.Series, c.AxisConfig), true
	default:
		return nil, false
	}
}

// vizSeriesLabels collects the free-form strings of a bar/area/line chart.
func vizSeriesLabels(series []slackapi.DataVisualizationDataSeries, axis slackapi.DataVisualizationAxisConfig) []vizLabel {
	out := make([]vizLabel, 0, len(axis.Categories)+2)
	for i, s := range series {
		out = append(out, vizLabel{path: fmt.Sprintf("series[%d].name", i), value: s.Name})
		for j, p := range s.Data {
			out = append(out, vizLabel{path: fmt.Sprintf("series[%d].data[%d].label", i, j), value: p.Label})
		}
	}
	for i, c := range axis.Categories {
		out = append(out, vizLabel{path: fmt.Sprintf("axis_config.categories[%d]", i), value: c})
	}
	return append(out,
		vizLabel{path: "axis_config.x_label", value: axis.XLabel, axis: true},
		vizLabel{path: "axis_config.y_label", value: axis.YLabel, axis: true})
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
		switch el.(type) {
		case *slackapi.RichTextSectionBroadcastElement, *slackapi.RichTextSectionUserGroupElement:
			// broadcast = @channel/@here/@everyone; usergroup = <!subteam^S…>,
			// which notifies every member — both are mass-notification pings.
			return true
		}
	}
	return false
}

func checkBroadcast(loc, typ, text string) error {
	if containsBroadcastMention(text) {
		return blockErrf(loc, typ, "broadcast or usergroup mentions (@channel, @here, @everyone, <!subteam^…>) are not allowed in agent-authored components")
	}
	return nil
}

// containsBroadcastMention reports whether text carries a Slack
// mass-notification span. Slack writes broadcasts as <!channel>, <!here>, or
// <!everyone>, and a usergroup ping (which notifies every member) as
// <!subteam^S…>, all optionally with a |label — so matching the opening token
// catches every form.
func containsBroadcastMention(text string) bool {
	for _, tok := range []string{"<!channel", "<!here", "<!everyone", "<!subteam^"} {
		if strings.Contains(text, tok) {
			return true
		}
	}
	return false
}
