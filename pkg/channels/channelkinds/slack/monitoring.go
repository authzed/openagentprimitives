// pkg/channels/channelkinds/slack/monitoring.go
//
// monitoringSender is the slack kind's MonitoringSender. It posts one
// chat.postMessage per framework MonitoringEvent to the Slack channel
// configured on the monitoring Channel's spec.slack.outputDefaults.
package slack

import (
	"context"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// monitoringSender implements channelkinds.MonitoringSender for Slack.
type monitoringSender struct {
	deps   channelkinds.Deps
	client slackClient
}

func newMonitoringSender(deps channelkinds.Deps) *monitoringSender {
	return &monitoringSender{deps: deps, client: newSlackAPIClient(deps.Secret)}
}

// SendMonitoring posts the event to the monitoring Channel's configured
// destination (spec.slack.outputDefaults.channelId).
//
// The primary rendering is the container card built by buildMonitoringBlocks —
// the same tone-chip-led box every other Slack surface draws, with the source
// ref as the bold lead and the machine metadata in a muted context footer. The
// flat mrkdwn line (renderMonitoringText) rides along as the notification
// preview AND is the degrade target when a workspace or SDK refuses the
// container block, exactly as the notice sender degrades (interaction_notice.go,
// postNoticeReturningTS).
func (s *monitoringSender) SendMonitoring(ctx context.Context, ev channelevents.MonitoringEvent) error {
	if s.client == nil {
		return fmt.Errorf("slack monitoring: bot-token missing from credentials Secret")
	}
	channelID := ""
	if ch := s.deps.Channel; ch != nil && ch.Spec.Slack != nil && ch.Spec.Slack.OutputDefaults != nil {
		channelID = ch.Spec.Slack.OutputDefaults.ChannelID
	}
	if channelID == "" {
		return fmt.Errorf("slack monitoring: spec.slack.outputDefaults.channelId is required for a monitoring channel")
	}
	// text is both the lock-screen/preview fallback (rendered when no block
	// renders) and the retry payload below.
	text := slackapi.MsgOptionText(renderMonitoringText(ev), false)
	_, _, err := s.client.PostMessageContext(ctx, channelID,
		slackapi.MsgOptionBlocks(buildMonitoringBlocks(ev)...), text)
	if err != nil && isUnsupportedBlocksErr(err) {
		log.FromContext(ctx).Info("slack monitoring: container rejected; falling back to plain text",
			"source", ev.Source.Kind+"/"+ev.Source.Name, "condition", ev.Condition, "err", err.Error())
		_, _, err = s.client.PostMessageContext(ctx, channelID, text)
	}
	if err != nil {
		return fmt.Errorf("post monitoring message: %w", err)
	}
	return nil
}

// monitoringTone maps a MonitoringEvent onto the shared tone vocabulary, so a
// platform admin watching the monitoring channel reads the same colours as a
// user reading their own thread.
//
// A recovery outranks its level: an event that says "error, recovered" is good
// news, and painting it red would train readers to ignore red.
func monitoringTone(ev channelevents.MonitoringEvent) channelinteractions.Tone {
	switch {
	case ev.Transition == channelevents.MonitoringTransitionRecovered:
		return channelinteractions.ToneResolved
	case ev.Level == channelevents.MonitoringLevelError:
		return channelinteractions.ToneCritical
	case ev.Level == channelevents.MonitoringLevelInfo:
		return channelinteractions.ToneHousekeeping
	default:
		return channelinteractions.ToneDegraded
	}
}

// renderMonitoringText formats a MonitoringEvent as Slack mrkdwn, led by the
// tone chip.
//
// A monitoring event is never terminal from the reader's side: it reports
// something that happened elsewhere, and the admin can always act on it. So it
// draws a circle, like every other actionable message.
//
// Every publisher-supplied field is escaped through escapeSlackText before it
// is interpolated. The tone chip, the backticks, the blockquote markers and the
// italic hint are written by this function and are the ONLY live markup in the
// result.
//
// This matters because a MonitoringEvent's fields routinely carry text the
// platform did not author -- a provider's raw HTTP response body, an upstream
// MCP server's tool name, and (for the credential-update card,
// pkg/channels/channelsd/pipeline/credential_update.go) the AGENT's own explanation of
// why it wants a credential replaced. SendMonitoring posts with
// MsgOptionText(..., false), i.e. mrkdwn is INTERPRETED, so unescaped text
// containing `<https://attacker.example/x|Update credential>` would render as
// a genuine-looking HYPERLINK inside the blockquote, one line above the real
// remediation hint, on a channel that is now a shared-bot-token entry surface.
func renderMonitoringText(ev channelevents.MonitoringEvent) string {
	icon := ":" + toneChip(monitoringTone(ev), false) + ":"
	var b strings.Builder
	fmt.Fprintf(&b, "%s *%s %s* — `%s %s/%s`",
		icon, escapeSlackText(ev.Level), escapeSlackText(ev.Transition),
		escapeSlackText(ev.Source.Kind), escapeSlackText(ev.Source.Namespace),
		escapeSlackText(ev.Source.Name))
	// The raw condition type is demoted to a trailing, unbolded machine ref
	// rather than led with in bold: a type such as "PartialFailure" names its
	// axis after the bad state, so a bold type verdict made a recovery read as a
	// failure. See monitoringMachineRef.
	if ref := monitoringMachineRef(ev); ref != "" {
		fmt.Fprintf(&b, " — %s", ref)
	}
	// Body leads with the human-facing message so the reader sees WHAT happened,
	// not the machine condition type. The blockquote prefix is applied AFTER
	// escaping so escaping can never eat the "> " markers this function writes.
	if lead := monitoringLead(ev); lead != "" {
		fmt.Fprintf(&b, "\n> %s", strings.ReplaceAll(escapeSlackText(lead), "\n", "\n> "))
	}
	if ev.Hint != "" {
		fmt.Fprintf(&b, "\n_%s_", escapeSlackText(ev.Hint))
	}
	return b.String()
}

// monitoringSourceRef renders the source object as "kind ns/name" (or "kind
// name" when cluster-scoped) — the reader's handle for the thing that spoke.
func monitoringSourceRef(ev channelevents.MonitoringEvent) string {
	if ev.Source.Namespace == "" {
		return ev.Source.Kind + " " + ev.Source.Name
	}
	return ev.Source.Kind + " " + ev.Source.Namespace + "/" + ev.Source.Name
}

// monitoringLead is the body's human-facing headline: the condition's message
// when it has one, else the machine reason, else the raw condition type.
//
// It exists so the body never LEADS with the condition TYPE. A type such as
// RelationshipSource's "PartialFailure" names its axis after the BAD state, so
// on a recovery (green chip, "recovered" footer, "AllScopesSynced" reason) a
// type-led body read as a failure — the contradiction this fixes. A cleared
// condition also routinely carries an empty message (the RelationshipSource
// controller sets Message:"" on the good arm), so the reason and then the type
// are the fallbacks that keep the body from rendering blank.
func monitoringLead(ev channelevents.MonitoringEvent) string {
	switch {
	case ev.Summary != "":
		return ev.Summary
	case ev.Reason != "":
		return ev.Reason
	default:
		return ev.Condition
	}
}

// monitoringMachineRef is the demoted condition/reason identifier that rides the
// muted footer (card) or the header tail (flat text) — the machine metadata the
// body no longer leads with.
//
// The reason is appended only when Summary is present, i.e. when monitoringLead
// did NOT already fall back to the reason as the body lead, so the reason is
// never printed twice. Every part is escaped: the condition type is a closed
// vocabulary, but the reason can carry provider-derived text (see
// renderMonitoringText's escaping note), so both get escapeSlackText.
func monitoringMachineRef(ev channelevents.MonitoringEvent) string {
	if ev.Condition == "" {
		return ""
	}
	ref := escapeSlackText(ev.Condition)
	if ev.Summary != "" && ev.Reason != "" {
		ref += ": " + escapeSlackText(ev.Reason)
	}
	return ref
}

// buildMonitoringBlocks renders a MonitoringEvent into the shared container
// shape, so the monitoring channel reads like every other Slack surface:
//
//	container (non-collapsible, header divider)
//	  rich_text_title : tone chip · spacer · source ref (bold, code)
//	  child_blocks    : section  — human message (monitoringLead), hint
//	                    context  — muted: level · transition · category · condition[: reason]
//
// The tone chip carries severity as colour, so the source ref — the thing an
// admin is actually scanning for — is the lead, the body leads with the
// human-facing message (never the raw condition TYPE, which names its axis
// after the bad state and so read as a failure on a recovery — see
// monitoringLead), and the level/transition/category and the demoted condition
// machine ref all drop to the muted footer.
//
// Untrusted fields (Reason, Summary, Hint, Category) are escaped through
// escapeSlackText before they reach an mrkdwn sink, for the reason
// renderMonitoringText documents at length: a MonitoringEvent routinely carries
// text the platform did not author. The source ref rides a rich_text element,
// which Slack renders LITERALLY — the same inert sink noticeTitle relies on — so
// it needs no escaping there.
func buildMonitoringBlocks(ev channelevents.MonitoringEvent) []slackapi.Block {
	// A monitoring event is never terminal from the reader's side (see
	// renderMonitoringText), so it always draws a circle, not a square.
	chip := toneChip(monitoringTone(ev), false)

	var children []slackapi.Block

	var body strings.Builder
	// Lead with the human-facing message; the raw condition type is demoted to
	// the muted footer (monitoringFooter). A condition TYPE such as
	// "PartialFailure" names its axis after the bad state, so a type-led body
	// read as a failure even on a green recovery. See monitoringLead.
	if lead := monitoringLead(ev); lead != "" {
		body.WriteString(escapeSlackText(lead))
	}
	if ev.Hint != "" {
		fmt.Fprintf(&body, "\n_%s_", escapeSlackText(ev.Hint))
	}
	// Split on Slack's per-section cap, exactly as buildInteractionBlocks does:
	// one unbounded section makes chat.postMessage reject the whole message.
	if body.Len() > 0 {
		for _, chunk := range chunkForSlackSection(body.String(), slackSectionMaxRunes) {
			children = append(children, slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(slackapi.MarkdownType, chunk, false, false), nil, nil))
		}
	}

	// Muted footer: the machine metadata, in a context block (Slack renders it
	// small and grey — the "muted text" the notice provenance line uses).
	children = append(children, slackapi.NewContextBlock("",
		slackapi.NewTextBlockObject(slackapi.MarkdownType, monitoringFooter(ev), false, false)))

	return []slackapi.Block{newContainerBlock(monitoringTitle(chip, ev), children...)}
}

// monitoringTitle builds the container's rich_text_title: tone chip, spacer,
// then the source ref bold + code-styled so it stands out as the lead the way
// the backticked ref did in the flat rendering.
func monitoringTitle(chip string, ev channelevents.MonitoringEvent) *slackapi.RichTextBlock {
	return slackapi.NewRichTextBlock("",
		slackapi.NewRichTextSection(
			slackapi.NewRichTextSectionEmojiElement(chip, 0, nil),
			slackapi.NewRichTextSectionTextElement(chipSpacer, nil),
			slackapi.NewRichTextSectionTextElement(monitoringSourceRef(ev),
				&slackapi.RichTextSectionTextStyle{Bold: true, Code: true}),
		),
	)
}

// monitoringFooter is the muted context line: severity, transition, category
// and the demoted condition/reason machine ref — the fields the chip and the
// body do not already carry. Every part is escaped: Level and Transition are
// closed vocab, but Category is a free publisher string (and the machine ref's
// reason can be provider-derived), so both get the same treatment as any other
// untrusted field.
func monitoringFooter(ev channelevents.MonitoringEvent) string {
	parts := []string{escapeSlackText(ev.Level), escapeSlackText(ev.Transition)}
	if ev.Category != "" {
		parts = append(parts, escapeSlackText(ev.Category))
	}
	// The raw condition type (and its reason) rides here now, demoted from the
	// body so a recovery is not headlined by a failure-named type.
	if ref := monitoringMachineRef(ev); ref != "" {
		parts = append(parts, ref)
	}
	return strings.Join(parts, " · ")
}
