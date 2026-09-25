// pkg/channels/channelkinds/slack/interaction_public_note.go
//
// The public "approval pending" note for the generic Interaction renderer:
// a non-actionable message visible to the whole conversation, posted
// alongside the private prompt when Audience.PublicNote is set.
//
// The note carries two kinds of text with opposite requirements, and
// publicNoteText is the one place they meet: the publisher's BODY is untrusted
// (tool_approval's is the summarizer LLM's sentence over agent-controlled tool
// arguments) and must be inert, while the approver MENTIONS this kind resolves
// must stay live or the channel is not told who it is waiting on. Both of the
// note's sinks parse Slack markup — the blocks section is mrkdwn, and the
// plain-text field is posted with escape=false — as does the ticker's
// re-render (interaction_ticker.go), so there is no sink here that forgives an
// unescaped body.
package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// publicNoteText composes the exact text a public note carries: the
// publisher's sentence made INERT, then the mentions THIS kind composed, left
// live. Every public note is built here; postPublicNote takes the result.
//
// The two steps live in ONE function because their ORDER is the security
// property and a comment cannot enforce an order:
//
//   - Sweep AFTER composing and the kind's own "<@U123>" renders as
//     "&lt;@U123&gt;", so the "Waiting on …" clause names nobody clickably.
//   - Skip the sweep and a publisher-supplied
//     "<https://attacker.example/x|Approve this request>" renders as a live
//     hyperlink in a PUBLIC post styled as the platform's own "Approval
//     pending" card, one line above the real approver mentions.
//
// inertProse, both halves: a bare "https://attacker.example/x" in the same
// sentence needs no markup at all, and this is the one surface where a forged
// action is shown to the whole room rather than to one expecting approver.
//
// The body is genuinely untrusted, not merely publisher-authored —
// tool_approval's is the summarizer LLM's one-liner over agent-controlled tool
// arguments.
func publicNoteText(body, mentions string) string {
	return withApproverClause(inertProse(body), mentions)
}

// postPublicNote posts an inert, non-actionable note to the session's
// channel/thread. Never a DM (the note is deliberately public). Returns where
// it landed so a later applied edit can update it. A session with no channel
// context (nil Channel, or an empty channel_id — the DM-only case) is a
// graceful skip, not an error: the note has no public surface to land in.
//
// body must come from publicNoteText: BOTH sinks below parse Slack markup —
// the blocks section is mrkdwn, and MsgOptionText's escape=false means slack-go
// does not escape the plain-text field for us — so this function cannot make
// the body inert itself without also killing the live mentions the caller
// composed into it.
func (s *interactionSender) postPublicNote(ctx context.Context, sess channelkinds.SessionInfo, requestRef, body string) (deliveryRef, error) {
	channelID := ""
	if sess.Channel != nil {
		channelID = sess.Channel.External["channel_id"]
	}
	if channelID == "" {
		// No channel context → no public surface to post the note to. A graceful
		// skip, not an error (see the doc comment above) — but still logged, same
		// as the other graceful-skip paths in this sender (e.g. sendRequest's
		// non-Slack-recipient skip), so an operator can see the note was
		// deliberately dropped rather than silently lost.
		log.FromContext(ctx).Info("interaction sender: no channel context; public note skipped",
			"session", sess.Namespace+"/"+sess.Name, "requestRef", requestRef)
		return deliveryRef{}, nil // no channel context → no public surface; skip (not an error)
	}
	opts := []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(publicNoteBlocks(body)...),
		slackapi.MsgOptionText(publicNoteNotifyText(body), false),
	}
	if ts := effectiveOutboundThreadTS(sess); ts != "" {
		opts = append(opts, slackapi.MsgOptionTS(ts))
	}
	_, ts, err := s.client.PostMessageContext(ctx, channelID, opts...)
	if err != nil && isUnsupportedBlocksErr(err) {
		log.FromContext(ctx).Info("interaction sender: public-note container rejected; falling back to a plain section",
			"requestRef", requestRef, "err", err.Error())
		opts[0] = slackapi.MsgOptionBlocks(slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, publicNoteNotifyText(body), false, false), nil, nil))
		_, ts, err = s.client.PostMessageContext(ctx, channelID, opts...)
	}
	if err != nil {
		return deliveryRef{}, fmt.Errorf("post public note (requestRef=%s): %w", requestRef, err)
	}
	return deliveryRef{ChannelID: channelID, TS: ts}, nil
}

// publicNoteLead is the fixed headline every "approval pending" note carries.
//
// Fixed rather than publisher-supplied because the note's job in a busy thread
// is to answer "is this thread blocked?" at a glance; the publisher's sentence
// says what is being asked and goes in the body under it.
const publicNoteLead = "Approval pending"

// publicNoteBlocks renders a public "approval pending" note in the shape every
// other interaction uses.
//
// Tone is WAITING — the thread is blocked on a person, which is exactly the
// distinction that tone exists to draw, and the reason it is not the routine
// tone the underlying prompt carries. The prompt asks someone to decide; this
// tells everyone else they are waiting on a human.
//
// Not terminal: an approval that has not happened yet is the opposite of over.
func publicNoteBlocks(body string) []slackapi.Block {
	return buildInteractionBlocks(
		channelevents.InteractionRequestPayload{Lead: publicNoteLead, Body: body},
		channelinteractions.Category{Tone: channelinteractions.ToneWaiting},
		"")
}

// publicNoteNotifyText is the plain-text form: the push preview, and the
// degraded rendering if Slack ever refuses the container.
func publicNoteNotifyText(body string) string {
	if strings.TrimSpace(body) == "" {
		return publicNoteLead + "."
	}
	return publicNoteLead + " — " + body
}

// approverMentions renders the approvers a public note is waiting on as Slack
// mentions, comma-joined.
//
// The channel needs to know WHO it is waiting on, or it cannot tell whether
// the answer is coming from someone in the room, someone who has left, or the
// reader themselves. The publisher cannot supply this: the runner holds only
// canonical SpiceDB subjects, and canonical → "<@U123>" resolution needs a
// Slack client, so naming the approver is necessarily the channel kind's job.
//
// Returns "" when there is nobody to name or nothing resolves — the note is
// still posted, just without the "waiting on" clause. A note that vanished
// because a lookup failed would be worse than one that is merely vaguer.
//
// resolved carries the canonical→user_id answers the caller's delivery loop
// already paid for. An email-typed canonical costs a live users.lookupByEmail
// and this runs synchronously on channelsd's single delivery goroutine, so
// re-deriving what is already known is a round trip per approver for nothing.
// A nil map is fine — every approver is then resolved live.
func approverMentions(
	ctx context.Context,
	cli slackClient,
	approvers []channelevents.ExternalIdentity,
	resolved map[identity.CanonicalUserID]string,
	logger logr.Logger,
	requestRef string,
) string {
	parts := make([]string, 0, len(approvers))
	for _, a := range approvers {
		if a.Kind != "slack" {
			continue // nothing this renderer can turn into a mention
		}
		canon, err := a.Principal().AllowSynthetic().Canonical()
		if err != nil {
			logger.Info("interaction sender: public-note approver canonical derivation failed; omitting",
				"requestRef", requestRef, "err", err.Error())
			continue
		}
		slackUserID, ok := resolved[canon]
		if !ok {
			slackUserID, err = resolveSlackUserIDFromCanonical(ctx, cli, canon)
			if err != nil {
				logger.Info("interaction sender: public-note approver resolution failed; omitting",
					"requestRef", requestRef, "err", err.Error())
				continue
			}
		}
		parts = append(parts, "<@"+slackUserID+">")
	}
	return strings.Join(parts, ", ")
}

// withApproverClause appends "Waiting on <@…>." to a public note body.
//
// Appended rather than interpolated because the body is publisher-authored
// copy in a package that has no Slack client — the publisher writes what is
// being asked, and the channel adds who is being asked.
//
// Composition only: the body must already be inert. Call it through
// publicNoteText, which is what pairs the escape with this append in the one
// order that keeps the publisher's sentence inert and these mentions live.
func withApproverClause(body, mentions string) string {
	if mentions == "" {
		return body
	}
	return body + "\nWaiting on " + mentions + "."
}
