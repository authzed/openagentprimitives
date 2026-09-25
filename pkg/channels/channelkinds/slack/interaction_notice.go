// pkg/channels/channelkinds/slack/interaction_notice.go
//
// Slack rendering for ZERO-ACTION notice categories (channelinteractions
// Category.Notice). A notice rides the same KindInteractionRequest wire as a
// prompt, so this is a branch inside the interaction sender's block builder,
// not a second sender.
//
// The shape below is not a design preference — it is what a live workspace
// accepts. Slack offers NO message-surface severity primitive: `alert` is
// modal-only, a `container` is rejected inside an attachment, and top-level
// blocks render above attachments anyway, so colour and structure cannot be
// combined. See NOTES.md for the full matrix and how to re-probe it.
//
// Tone therefore rides as an emoji chip in the container's `rich_text_title`,
// assigned HERE from the Tone enum and the category's Terminal flag. A caller
// cannot supply one, which is what keeps the vocabulary closed.
package slack

import (
	"context"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// toneChip maps a message's tone and finality to the Slack emoji NAME drawn
// beside its lead. Two orthogonal axes ride one glyph:
//
//   - COLOUR is the tone: what kind of concern this is.
//   - SHAPE is finality: ■ nothing further will happen · ● there is something
//     here for you, an action or knowledge.
//
// Shape deliberately does not encode prompt-vs-notice. The presence of buttons
// already says that, so spending the axis on it would carry no information;
// spending it on "is this over?" tells a reader whether to keep waiting, which
// nothing else on the message says.
//
// The whole mapping is Slack's alone. The channel-agnostic registry carries a
// Tone and a Terminal flag, never an emoji name, so adding a channel never
// means touching the registry.
func toneChip(t channelinteractions.Tone, terminal bool) string {
	if terminal {
		switch t {
		case channelinteractions.ToneCritical:
			return "large_red_square"
		case channelinteractions.TonePrivacy:
			return "large_purple_square"
		case channelinteractions.ToneDegraded:
			return "large_orange_square"
		case channelinteractions.ToneWaiting:
			return "large_yellow_square"
		case channelinteractions.ToneResolved:
			return "large_green_square"
		case channelinteractions.ToneHousekeeping:
			return "white_large_square"
		// Black reads as "switched off", which is why ToneUnavailable takes it
		// rather than brown: white is already housekeeping's, and those two
		// were the only unused chips left in the palette.
		case channelinteractions.ToneUnavailable:
			return "black_large_square"
		default: // ToneRoutine
			return "large_blue_square"
		}
	}
	switch t {
	case channelinteractions.ToneCritical:
		return "red_circle"
	case channelinteractions.TonePrivacy:
		return "large_purple_circle"
	case channelinteractions.ToneDegraded:
		return "large_orange_circle"
	case channelinteractions.ToneWaiting:
		return "large_yellow_circle"
	case channelinteractions.ToneResolved:
		return "large_green_circle"
	case channelinteractions.ToneHousekeeping:
		return "white_circle"
	case channelinteractions.ToneUnavailable:
		return "black_circle"
	default: // ToneRoutine
		return "large_blue_circle"
	}
}

// glyphChip maps an optional semantic Glyph override to a Slack emoji name.
// An unrecognised glyph returns "" so the caller falls back to the tone chip —
// a renderer must never be the reason a message fails to draw.
//
// Slack does NOT validate emoji names: an unknown name is accepted by the API
// and rendered as literal `:name:` text. That silent failure is why Glyph is a
// closed typed set upstream rather than a string a caller passes.
func glyphChip(g channelinteractions.Glyph) string {
	switch g {
	case channelinteractions.GlyphMoney:
		return "moneybag"
	case channelinteractions.GlyphClock:
		return "hourglass_flowing_sand"
	case channelinteractions.GlyphWarning:
		return "warning"
	default:
		return ""
	}
}

// noticeChip picks the emoji name: the category's glyph override when it
// declares one, else the tone chip.
func noticeChip(t channelinteractions.Tone, terminal bool, g channelinteractions.Glyph) string {
	if name := glyphChip(g); name != "" {
		return name
	}
	return toneChip(t, terminal)
}

// chipSpacer separates the chip from the lead.
//
// Two NON-BREAKING spaces, not two ordinary ones: Slack collapses runs of
// ordinary whitespace in a rich_text element to a single space, even when the
// run is its own element, so "  " and " " render identically. U+00A0 survives.
// Undocumented; found by posting both.
const chipSpacer = "  "

// noticeTitle builds the container's rich_text_title: chip, spacer, then the
// lead in bold.
//
// The spacer is its OWN element because leading whitespace inside the lead's
// element is trimmed outright — a separate element is what keeps any gap at
// all, and chipSpacer is what makes that gap two-wide.
//
// rich_text_title is also the ONLY title form that renders an emoji: the
// plain_text `title` prints `:large_red_square:` literally even with
// emoji:true, and `subtitle` renders emoji but drops mrkdwn code spans. Both
// undocumented; both found by posting.
func noticeTitle(chip, lead string) *slackapi.RichTextBlock {
	return slackapi.NewRichTextBlock("",
		slackapi.NewRichTextSection(
			slackapi.NewRichTextSectionEmojiElement(chip, 0, nil),
			slackapi.NewRichTextSectionTextElement(chipSpacer, nil),
			slackapi.NewRichTextSectionTextElement(lead, &slackapi.RichTextSectionTextStyle{Bold: true}),
		),
	)
}

// buildInteractionBlocks renders any interaction — prompt or notice — to the
// verified container shape:
//
//	container (non-collapsible, header divider)
//	  rich_text_title : tone chip · spacer · bold lead
//	  child_blocks    : section  — body, then bold NextStep on its own line
//	                    section  — inert Excerpt, when present
//	                    actions  — buttons, when the payload has any
//	                    divider
//	                    context  — provenance
//
// sessRef is the encoded session reference the buttons round-trip.
func buildInteractionBlocks(
	p channelevents.InteractionRequestPayload,
	cat channelinteractions.Category,
	sessRef string,
) []slackapi.Block {
	var children []slackapi.Block

	// Body + NextStep. NextStep is bold and on its own line on every surface:
	// it is what a stuck user is scanning for.
	//
	// Everything here is interpolated VERBATIM, deliberately: this renderer is
	// shared by callers that compose their own live markup into these fields —
	// buildInteractionAppliedBlocks prepends a verdict naming the decider as a
	// real `<@U…>` mention, publicNoteBlocks a body carrying approver mentions.
	// Publisher-supplied text is made inert one layer up, at the wire-payload
	// boundary in buildInteractionRequestBlocks (escapePublisherPayload), where
	// it can still be told apart from markup this kind composed.
	var body strings.Builder
	if b := strings.TrimSpace(p.Body); b != "" {
		body.WriteString(b)
	}
	if ns := strings.TrimSpace(p.NextStep); ns != "" {
		if body.Len() > 0 {
			body.WriteString("\n\n")
		}
		body.WriteString("*" + ns + "*")
	}
	for _, f := range p.Fields {
		// Structure wins when the publisher sent it — same fallback contract
		// webchat's FieldItems honours: Value is the always-present fallback,
		// Items is what a surface with a design system renders instead, and
		// it is the only place a line's tier (and its external marker) exists.
		if len(f.Items) > 0 {
			fmt.Fprintf(&body, "\n*%s*:%s", f.Label, interactionFieldItemsMrkdwn(f.Items, 0))
			continue
		}
		fmt.Fprintf(&body, "\n• *%s*: %s", f.Label, f.Value)
	}
	// Split on Slack's per-section cap, exactly as the reply path does.
	//
	// One unbounded section makes chat.postMessage reject the ENTIRE message
	// with invalid_blocks, and for an interaction that is not a truncated
	// message but a stopped conversation: the card never posts, nobody is
	// asked, and the runner waits out its approval timeout on a decision no
	// human ever saw. A plan-gate card reaches this on ordinary input — its
	// What carries the phase's whole ceiling plus a line per requested
	// resource, and a phase may declare 128 of them.
	//
	// chunkForSlackSection never drops content, which is the property that
	// matters over truncation: the resources land at the END of the What, so
	// cutting to fit would remove precisely what the approver is consenting to.
	if body.Len() > 0 {
		for _, chunk := range chunkForSlackSection(body.String(), slackSectionMaxRunes) {
			children = append(children, slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(slackapi.MarkdownType, chunk, false, false), nil, nil))
		}
	}

	// Excerpt: UNTRUSTED content. CONTRACT (channelevents.InteractionExcerpt):
	// Label AND Content render inert — fenced and escaped, never live markup,
	// never adjacent to an action in a way that lets it impersonate one.
	if p.Excerpt != nil {
		label := strings.TrimSpace(p.Excerpt.Label)
		if label == "" {
			label = "Flagged content (untrusted — shown as inert text, not interpreted)"
		}
		children = append(children, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType,
				inertExcerpt(label)+":\n```\n"+inertExcerpt(p.Excerpt.Content)+"\n```", false, false), nil, nil))
	}

	if elements := interactionActionElements(p, sessRef); len(elements) > 0 {
		children = append(children, slackapi.NewActionBlock("interaction_actions", elements...))
	}

	// Provenance, separated by a rule. It lives in a trailing context child
	// rather than the container's subtitle because subtitle silently drops
	// code spans — `agents/sre-bot-4f2c` would render with visible backticks.
	if prov := noticeProvenance(p); prov != "" {
		children = append(children, slackapi.NewDividerBlock())
		children = append(children, slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject(slackapi.MarkdownType, prov, false, false)))
	}

	// A container needs at least one child; a message with nothing but a lead
	// is legal, so give it one rather than emitting an invalid block.
	if len(children) == 0 {
		children = append(children, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, " ", false, false), nil, nil))
	}

	chip := noticeChip(cat.Tone, cat.Terminal, cat.Glyph)
	return []slackapi.Block{newContainerBlock(noticeTitle(chip, p.Lead), children...)}
}

// noticeProvenance is the muted footer line: which session this came from.
// Kept to session identity only — a notice's footer is for locating the thing
// that spoke, not for restating the message.
func noticeProvenance(p channelevents.InteractionRequestPayload) string {
	ns, name := p.AgentSessionRef.Namespace, p.AgentSessionRef.Name
	if ns == "" || name == "" {
		return ""
	}
	return fmt.Sprintf("session `%s/%s`", ns, name)
}

// buildNoticeFallbackBlocks is the degrade target when Slack rejects the
// container — the block is one month old and absent from slack-go, so this
// path is a live possibility rather than defensive dressing.
//
// It uses only section + context, which this codebase has posted since day
// one. Tone survives as a shortcode marker, so a degraded notice still
// distinguishes a dead session from an FYI. Same discipline
// sender_plan.go applies for plan/task_card.
func buildNoticeFallbackBlocks(
	p channelevents.InteractionRequestPayload,
	tone channelinteractions.Tone,
	terminal bool,
	sessRef string,
) []slackapi.Block {
	var text strings.Builder
	if marker := fallbackMarker(tone); marker != "" {
		text.WriteString(marker + " ")
	}
	// Body / NextStep / Fields are interpolated verbatim for the same reason as
	// buildInteractionBlocks above: this is the degrade target for the SAME
	// payload, so it must not disagree with the container about what is
	// already-composed markup and what is publisher text.
	//
	// Lead is the one field that CANNOT follow that rule, because the two
	// renderings do not share a sink: the container hands it to a rich_text
	// element Slack renders literally, and this one puts it in mrkdwn, where the
	// same string would open a live link or a channel-wide ping. That asymmetry
	// is exactly why escapePublisherPayload leaves Lead alone (escaping it would
	// show "&amp;" in the container's title), which makes this the last place
	// the mrkdwn copy can be made inert. Double-escaping is impossible by
	// construction: no caller ever hands this function an escaped Lead.
	text.WriteString("*" + escapeSlackText(p.Lead) + "*")
	if b := strings.TrimSpace(p.Body); b != "" {
		text.WriteString("\n" + b)
	}
	if ns := strings.TrimSpace(p.NextStep); ns != "" {
		text.WriteString("\n\n*" + ns + "*")
	}
	for _, f := range p.Fields {
		fmt.Fprintf(&text, "\n• *%s*: %s", f.Label, f.Value)
	}

	blocks := []slackapi.Block{slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, text.String(), false, false), nil, nil)}

	if p.Excerpt != nil {
		label := strings.TrimSpace(p.Excerpt.Label)
		if label == "" {
			label = "Detail (untrusted — shown as inert text, not interpreted)"
		}
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType,
				inertExcerpt(label)+":\n```\n"+inertExcerpt(p.Excerpt.Content)+"\n```", false, false), nil, nil))
	}
	if len(p.Details) > 0 {
		blocks = append(blocks, slackapi.NewActionBlock("notice_actions",
			slackapi.NewButtonBlockElement(
				interactionDetailsActionID,
				encodeApprovalButtonValue(discInteractionDetails, p.RequestRef, "", sessRef),
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "Show Details", true, false),
			)))
	}
	if prov := noticeProvenance(p); prov != "" {
		blocks = append(blocks, slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject(slackapi.MarkdownType, prov, false, false)))
	}
	return blocks
}

// fallbackMarker is the tone word used when the container is unavailable.
// Mirrors the text floor's marker so a degraded Slack notice and a
// text-surface notice say the same thing.
//
// Shortcode circles rather than the tone chips, because this path renders in a
// plain section where a chip would carry no shape meaning anyway.
func fallbackMarker(t channelinteractions.Tone) string {
	switch t {
	case channelinteractions.ToneCritical:
		return ":red_circle:"
	case channelinteractions.TonePrivacy:
		return ":large_purple_circle:"
	case channelinteractions.ToneDegraded:
		return ":large_orange_circle:"
	case channelinteractions.ToneWaiting:
		return ":large_yellow_circle:"
	case channelinteractions.ToneResolved:
		return ":large_green_circle:"
	default: // ToneRoutine, ToneHousekeeping — deliberately unmarked
		return ""
	}
}

// noticeNotifyText is the plain-text push-notification preview. Slack shows it
// on a lock screen and in the channel list, where no block renders — so it
// must stand alone. Lead plus NextStep is the smallest thing that is still
// actionable.
//
// Every caller posts the result through MsgOptionText(_, false), which means
// slack-go does NOT escape the field for us and Slack parses it as mrkdwn — so
// the Lead is made inert here, exactly as buildNoticeFallbackBlocks does for
// the other mrkdwn sink on this same path, and notifyOption does for the wire
// path's. Leaving it live here made two sinks disagree about one payload.
//
// The rule is per-SINK, not per-trust. Lead is the one field
// escapePublisherPayload cannot neutralize at the payload boundary, because
// its card sink is a rich_text element Slack renders literally and entities
// there would be VISIBLE ("&amp;" for an ordinary ampersand) — so each mrkdwn
// sink escapes it itself. That is lossless: Slack decodes the entities back
// when it renders mrkdwn.
//
// NextStep is deliberately left live. Its sink is uniformly mrkdwn, so the
// trust decision for it is made once — escaped at the wire boundary for a
// published payload, live for the in-process notice copy this function
// renders, whose Args are trusted by contract (pkg/channels/notice).
func noticeNotifyText(p channelevents.InteractionRequestPayload) string {
	if ns := strings.TrimSpace(p.NextStep); ns != "" {
		return escapeSlackText(p.Lead) + " — " + ns
	}
	return escapeSlackText(p.Lead)
}

// noticeMsgOptions renders a notice into the MsgOptions for a chat.postMessage,
// for the Slack-local callers that need the posted message's ts back and so
// cannot use the fire-and-forget posters.
//
// fallback selects the degrade rendering, for a retry after Slack rejects the
// container. Callers post with fallback=false first and retry with true only on
// isUnsupportedBlocksErr.
func noticeMsgOptions(
	n *notice.Notice,
	sess channelevents.SessionRef,
	requestRef string,
	fallback bool,
) ([]slackapi.MsgOption, error) {
	pl, err := n.Payload(sess, requestRef)
	if err != nil {
		return nil, err
	}
	tone, terminal, glyph, err := n.Style()
	if err != nil {
		return nil, err
	}
	blocks := buildInteractionBlocks(pl, channelinteractions.Category{Tone: tone, Terminal: terminal, Glyph: glyph}, "")
	if fallback {
		blocks = buildNoticeFallbackBlocks(pl, tone, terminal, "")
	}
	return []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(blocks...),
		slackapi.MsgOptionText(noticeNotifyText(pl), false),
	}, nil
}

// postNoticeReturningTS posts a notice and returns the message ts, degrading to
// the plain rendering if Slack rejects the container.
//
// A render or build failure falls back to posting the lead as plain text: these
// callers are establishing a thread root, and a thread that fails to start
// because its framing message would not render is a far worse outcome than an
// unstyled framing message.
func postNoticeReturningTS(
	ctx context.Context,
	cli slackClient,
	channelID string,
	n *notice.Notice,
	sess channelevents.SessionRef,
	requestRef string,
	extra ...slackapi.MsgOption,
) (string, error) {
	opts, err := noticeMsgOptions(n, sess, requestRef, false)
	if err != nil {
		log.FromContext(ctx).Info("slack: notice render failed; posting the lead as plain text",
			"category", n.Category(), "err", err.Error())
		// mrkdwn sink, so the Lead is escaped here — the same treatment
		// noticeNotifyText gives it on the non-degraded path just below, and
		// the per-sink rule inert.go states. The two must agree: a degraded
		// rendering that is MORE live than the one it replaced is the worst of
		// both.
		_, ts, perr := cli.PostMessageContext(ctx, channelID,
			append([]slackapi.MsgOption{slackapi.MsgOptionText(escapeSlackText(n.Args().Lead), false)}, extra...)...)
		return ts, perr
	}
	_, ts, err := cli.PostMessageContext(ctx, channelID, append(opts, extra...)...)
	if err != nil && isUnsupportedBlocksErr(err) {
		log.FromContext(ctx).Info("slack: notice container rejected; falling back to plain blocks",
			"category", n.Category(), "err", err.Error())
		fb, ferr := noticeMsgOptions(n, sess, requestRef, true)
		if ferr != nil {
			return "", ferr
		}
		_, ts, err = cli.PostMessageContext(ctx, channelID, append(fb, extra...)...)
	}
	return ts, err
}
