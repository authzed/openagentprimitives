// pkg/channels/channelkinds/slack/notice_post.go
//
// Posting a notice from inside the Slack listener, without a wire round-trip.
//
// Most notices are published by a component that owns a NATS handle
// (notice.Publish → KindInteractionRequest → the outbound relay → this
// kind's interaction sender). The listener's inbound error paths are the
// exception: they are already holding the user's HTTP/socket event, there is
// often no session yet to publish against, and the reply must be ephemeral to
// the person who just typed. Round-tripping through NATS to tell someone
// "that didn't work" would add a failure mode to the failure handler.
//
// So those sites render the SAME notice through the SAME block builder and
// post it directly. Posting directly is the only thing that differs: the
// copy, the severity, and the blocks all still come from the registry and the
// shared renderer, so the listener never holds a message of its own.
package slack

import (
	"context"
	"errors"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// noticeRequestRef is the RequestRef used for a listener-posted notice.
//
// A notice has no decision leg, so nothing ever correlates against this — but
// the payload contract requires a non-empty value, and a fixed sentinel is
// honest about there being nothing to correlate. Minting a fresh id would
// imply a lifecycle that does not exist.
const noticeRequestRef = "notice"

// postNoticeEphemeral renders n through the shared notice renderer and posts
// it as an ephemeral message to userID.
//
// Every failure path here is loud. A notice is frequently the ONLY signal a
// user gets that something went wrong, so a notice that fails to render or
// fails to post must never be the second silent failure on top of the first:
//
//   - A build/render error logs at Info with the category, then falls through
//     to a plain-text post of the lead, so the user still learns something
//     happened.
//   - A post error is returned to the caller, which logs it.
//
// A suppressed notice posts nothing by design and logs its reason.
func (l *slackListener) postNoticeEphemeral(
	ctx context.Context,
	channelID, userID string,
	sess channelevents.SessionRef,
	n *notice.Notice,
) error {
	logger := log.FromContext(ctx)
	if n.IsSuppressed() {
		logger.Info("slack: notice suppressed, posting nothing",
			"reason", n.SuppressReason(), "channelID", channelID, "userID", userID)
		return nil
	}

	if l.api == nil {
		return errors.New("slack: client not configured")
	}

	pl, err := n.Payload(sess, noticeRequestRef)
	if err != nil {
		// A malformed notice is a programmer error, but the user is waiting.
		// Say something rather than nothing: the lead alone is still true.
		logger.Info("slack: notice payload invalid; falling back to plain text",
			"category", n.Category(), "err", err.Error())
		lead := n.Args().Lead
		if lead == "" {
			lead = "Something went wrong handling your message."
		}
		// Escaped HERE, not inside postEphemeralText: that helper's other
		// caller passes noticeNotifyText(pl), which has already escaped the
		// Lead, and escaping again would show a reader "&amp;lt;". This is the
		// per-sink rule inert.go states — the ephemeral's text field is
		// mrkdwn, so this sink escapes, exactly as its non-degraded sibling
		// does.
		return l.postEphemeralText(ctx, channelID, userID, escapeSlackText(lead))
	}

	tone, terminal, glyph, err := n.Style()
	if err != nil {
		logger.Info("slack: notice style unresolved; falling back to plain text",
			"category", n.Category(), "err", err.Error())
		return l.postEphemeralText(ctx, channelID, userID, noticeNotifyText(pl))
	}

	blocks := buildInteractionBlocks(pl, channelinteractions.Category{Tone: tone, Terminal: terminal, Glyph: glyph}, "")
	_, err = l.api.PostEphemeralContext(ctx, channelID, userID,
		slackapi.MsgOptionBlocks(blocks...),
		// The plain-text preview: what a push notification and the channel
		// list show, where no block renders.
		slackapi.MsgOptionText(noticeNotifyText(pl), false),
	)
	if err != nil && isUnsupportedBlocksErr(err) {
		// The container block is new and absent from slack-go; a workspace
		// that rejects it must still get the message. Same degrade discipline
		// sender_plan.go applies to plan/task_card.
		logger.Info("slack: notice container rejected; falling back to plain blocks",
			"category", n.Category(), "err", err.Error())
		_, err = l.api.PostEphemeralContext(ctx, channelID, userID,
			slackapi.MsgOptionBlocks(buildNoticeFallbackBlocks(pl, tone, terminal, "")...),
			slackapi.MsgOptionText(noticeNotifyText(pl), false),
		)
	}
	return err
}

// postNoticeInThread renders n and posts it visibly in the thread, for
// notices the whole conversation should see.
//
// A denial is posted here rather than ephemerally on purpose: when the bot
// refuses someone in a shared thread, coworkers who saw the request need to
// see why it went unanswered, or the bot just looks broken.
func (l *slackListener) postNoticeInThread(
	ctx context.Context,
	channelID, threadTS string,
	sess channelevents.SessionRef,
	n *notice.Notice,
) {
	logger := log.FromContext(ctx)
	if n.IsSuppressed() {
		logger.Info("slack: notice suppressed, posting nothing in thread",
			"reason", n.SuppressReason(), "channelID", channelID, "threadTS", threadTS)
		return
	}
	if l.api == nil {
		logger.Info("slack: cannot post notice in thread, client not configured",
			"category", n.Category(), "channelID", channelID)
		return
	}

	pl, err := n.Payload(sess, noticeRequestRef)
	if err != nil {
		logger.Info("slack: notice payload invalid; posting the lead as plain text",
			"category", n.Category(), "err", err.Error())
		// Escaped at the call site rather than in postInThread, for the same
		// reason as postNoticeEphemeral's fallback above: the helper's other
		// callers hand it text that is already inert.
		l.postInThread(ctx, channelID, threadTS, escapeSlackText(n.Args().Lead))
		return
	}
	tone, terminal, glyph, err := n.Style()
	if err != nil {
		logger.Info("slack: notice style unresolved; posting plain text",
			"category", n.Category(), "err", err.Error())
		l.postInThread(ctx, channelID, threadTS, noticeNotifyText(pl))
		return
	}

	opts := []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(buildInteractionBlocks(pl, channelinteractions.Category{Tone: tone, Terminal: terminal, Glyph: glyph}, "")...),
		slackapi.MsgOptionText(noticeNotifyText(pl), false),
	}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, _, err := l.api.PostMessageContext(ctx, channelID, opts...); err != nil {
		if isUnsupportedBlocksErr(err) {
			logger.Info("slack: notice container rejected in thread; falling back to plain blocks",
				"category", n.Category(), "err", err.Error())
			fb := []slackapi.MsgOption{
				slackapi.MsgOptionBlocks(buildNoticeFallbackBlocks(pl, tone, terminal, "")...),
				slackapi.MsgOptionText(noticeNotifyText(pl), false),
			}
			if threadTS != "" {
				fb = append(fb, slackapi.MsgOptionTS(threadTS))
			}
			if _, _, ferr := l.api.PostMessageContext(ctx, channelID, fb...); ferr != nil {
				logger.Info("slack: notice fallback post failed",
					"category", n.Category(), "err", ferr.Error())
			}
			return
		}
		// Never silent: a notice that fails to post is the second failure on
		// top of whatever prompted it, and the user sees neither.
		logger.Info("slack: notice post failed",
			"category", n.Category(), "channelID", channelID, "err", err.Error())
	}
}

// internalErrorNotice is the one definition of "we broke, not you", shared by
// every listener path that fails before a message reaches the agent.
//
// One definition means improving this sentence improves it on every path at
// once — which is the point, since these paths are the ones a user hits when
// something is already going wrong.
func internalErrorNotice() *notice.Notice {
	return notice.New(categories.InternalError, notice.Args{
		Lead:     "Couldn't process your message",
		Body:     "Something went wrong on our side — your message didn't reach the agent.",
		NextStep: "Send it again.",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

// sessionRefOf extracts the session a decision concerns, for the notice's
// provenance footer. A decision with no session (a refusal before one was
// resolved) yields a zero ref, and the renderer omits the footer rather than
// naming a session that does not exist.
func sessionRefOf(dec channelkinds.InboundDecision) channelevents.SessionRef {
	return channelevents.SessionRef{
		Namespace: dec.Session.Namespace,
		Name:      dec.Session.Name,
	}
}

// postEphemeralText posts a bare-text ephemeral through the listener's own
// API client.
//
// It goes through l.api (the listenerAPIClient interface) rather than the
// concrete *slackapi.Client, because a listener whose client is a test double
// still has to deliver: a notice that renders nowhere under the fake is a
// notice whose delivery no test can prove.
func (l *slackListener) postEphemeralText(ctx context.Context, channelID, userID, text string) error {
	if l.api == nil {
		return errors.New("slack: client not configured")
	}
	_, err := l.api.PostEphemeralContext(ctx, channelID, userID, slackapi.MsgOptionText(text, false))
	return err
}
