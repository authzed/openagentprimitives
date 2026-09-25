// pkg/channels/channelkinds/slack/interaction_applied.go
//
// Slack rendering for a RESOLVED interaction — the edit that replaces a
// decision prompt once it has been approved, denied, or has expired.
//
// A resolved card is the same house container as the prompt it replaces, and it
// keeps the prompt's detail. That is not decoration: the Applied payload carries
// only a verdict (Category, Outcome, OutcomeText, DecidedBy, Reason — no lead,
// no fields), so rendering it alone answers "what happened" while destroying "to
// what". Most categories leave OutcomeText empty, so a verdict-only card
// degrades all the way to a wire constant behind a tone chip.
//
// The detail comes from the in-process interactionDeliveryStore, which already
// caches the prompt/note refs this edit targets — making the detail exactly as
// durable as the ability to edit at all. A channelsd restart loses both
// together, and the no-request path below degrades rather than failing to
// render.
//
// pkg/web/webui/chat/ui/InteractionCard.tsx does the same, keeping
// request.lead/body/fields/excerpt on screen and swapping only the action row,
// so both surfaces describe a resolved approval alike.
package slack

import (
	"strings"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// appliedOutcomeLabel is the verdict word for a resolved interaction.
//
// The rules live in categories.OutcomeLabel, shared with the oap chat TUI, so
// the two surfaces cannot drift on what a resolved prompt says.
func appliedOutcomeLabel(p channelevents.InteractionAppliedPayload) string {
	return categories.OutcomeLabel(p.Category, p.OutcomeText, p.Outcome)
}

// appliedDeciderMention renders who decided, as a native Slack mention when
// the decider is a Slack user (ExternalID is the raw slack id, per
// ExternalIdentity's contract) and their email otherwise.
//
// Empty for a nil DecidedBy — a timeout has no decider, and "Expired by
// nobody" is worse than "Expired".
func appliedDeciderMention(e *channelevents.ExternalIdentity) string {
	if e == nil {
		return ""
	}
	if e.Kind == identity.KindSlack && e.ExternalID != "" {
		return "<@" + string(e.ExternalID) + ">"
	}
	if e.Email != "" {
		return string(e.Email)
	}
	return ""
}

// appliedVerdictLine is the one line a resolved card adds to the prompt it
// replaces: bold verdict, who decided, and why when a handler gave a reason.
//
// It sweeps the label and the reason and THEN composes the decider mention, and
// that ORDER is the security property — the same rule as publicNoteText and
// buildInteractionAppliedBlocks below:
//
//   - Neither value is platform copy on every path. On queued_messages both are
//     the RUNNER's: the interrupt bridge sets OutcomeText to "Couldn't interrupt
//     — " + a reason arriving verbatim on .out.interrupt_applied. This line
//     lands in a MarkdownType section, so unswept it renders
//     `<https://attacker.example/x|Retry>` as a live hyperlink inside a card
//     saying a named human already decided — and sendDecisionApplied writes
//     these same blocks to the PUBLIC note as well as the prompt.
//   - inertProse, both halves: the verdict is composed into the SAME text object
//     as the cached Body escapePublisherPayload just swept, so escaping alone
//     would put one defused string beside one live one on one card.
//   - The decider mention is composed AFTER so it stays clickable — it is markup
//     THIS kind resolved from a structured identity, and sweeping the composed
//     line would render "&lt;@U_APPROVER&gt;" and stop naming who decided.
//
// escapePublisherPayload sweeps the cached REQUEST payload and this verdict is
// composed afterwards, so this is the only pass over these two — and exactly
// one pass, since the sweep is not idempotent.
func appliedVerdictLine(p channelevents.InteractionAppliedPayload) string {
	var b strings.Builder
	b.WriteString("*" + inertProse(appliedOutcomeLabel(p)) + "*")
	if m := appliedDeciderMention(p.DecidedBy); m != "" {
		b.WriteString(" by " + m)
	}
	if r := strings.TrimSpace(p.Reason); r != "" {
		b.WriteString(" — " + inertProse(r))
	}
	return b.String()
}

// buildInteractionAppliedBlocks renders a resolved decision as the house
// container: the original prompt's lead, body, and fields, led by the verdict,
// with the buttons stripped.
//
// req is the cached request this Applied resolves, or nil when the delivery
// store has no record of it (a channelsd restart between prompt and decision).
// With nil the card degrades to the verdict alone — still a container, because
// shape is what makes a thread readable and it costs nothing to keep.
//
// The verdict goes at the TOP of the body rather than after the fields: the
// container title carries the OUTCOME ("Approved" / "Denied"), so a reader
// scanning a resolved thread wants who-and-why next, with the particulars
// (the original fields) under it.
//
// Tone is the outcome's (approved resolved-green, denied degraded-orange,
// expired waiting-yellow) and NON-terminal, matching credential_link's resolved
// card (buildInteractionResolvedBlocks) — the two resolved renderings draw the
// same shape, so whether a settled interaction should take the terminal square
// is one decision about both.
//
// This is a WIRE BOUNDARY like buildInteractionRequestBlocks: req is the
// payload exactly as it arrived over NATS, handed back verbatim by the delivery
// store, so it is made inert HERE. Sweep and composition live in ONE function
// because their ORDER is the security property:
//
//   - Sweep first, or the publisher's Body/Fields forge an action on a card the
//     reader is told a named human already decided. A tool_approval whose
//     Fields carry LLM-supplied argument values, or a credential-update card
//     whose Body is the agent's own sentence, renders
//     `<https://attacker.example/x|Update credential>` as a REAL hyperlink the
//     moment someone clicks — the same string the pending card neutralised,
//     re-livened on all three surfaces this card is pushed to (response_url
//     copy, recorded prompt, PUBLIC note).
//   - Compose second, so appliedVerdictLine's `<@U…>` decider mention stays
//     clickable rather than rendering "&lt;@U_APPROVER&gt;".
//
// The delivery store keeps the wire payload verbatim rather than a pre-escaped
// copy: it is the record of what was published, its other readers are not all
// mrkdwn sinks, and the sweep is not idempotent.
func buildInteractionAppliedBlocks(
	p channelevents.InteractionAppliedPayload,
	req *channelevents.InteractionRequestPayload,
) []slackapi.Block {
	pl := channelevents.InteractionRequestPayload{AgentSessionRef: p.AgentSessionRef}
	if req != nil {
		pl = escapePublisherPayload(*req)
	}

	// The interaction is settled: a live Approve button on a resolved card
	// invites a click that can only ever be rejected, and Show-Details is
	// backed by a delivery record that sendDecisionApplied is about to drop.
	pl.Actions = nil
	pl.Details = nil
	// NextStep belonged to the pending prompt ("Approve or deny below") — it
	// names an action that no longer exists.
	pl.NextStep = ""

	verdict := appliedVerdictLine(p)
	if b := strings.TrimSpace(pl.Body); b != "" {
		pl.Body = verdict + "\n\n" + b
	} else {
		pl.Body = verdict
	}
	// The applied title reflects the DECISION, from the same source the tone chip
	// does. The cached request's Lead is a PENDING status word in production —
	// "Approval needed" on the prompt (toolApprovalLead), "Approval pending" on
	// the public note — so keeping it rendered a settled card whose title still
	// said pending while its chip had gone resolved-green: two views of one
	// interaction, drawn from different sources, contradicting each other. What
	// the interaction was ABOUT survives in the fields and body kept above; the
	// title is the status line, and a settled interaction's status is its outcome.
	pl.Lead = channelevents.OutcomeHeadline(p.Outcome)

	return buildInteractionBlocks(pl,
		channelinteractions.Category{Tone: outcomeTone(p.Outcome)}, "")
}
