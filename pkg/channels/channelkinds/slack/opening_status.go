// pkg/channels/channelkinds/slack/opening_status.go
//
// Slack's implementation of channelkinds.OpeningMessageEditor: edits the
// pinned opening message a triggered session posts at thread-root in place,
// via chat.update, as the session's badge/body/link evolve. See
// channelkinds.OpeningMessageContent for the field contract.
package slack

import (
	"context"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

var _ channelkinds.OpeningMessageEditor = (*slackSender)(nil)

// openingBadgeLabel maps the badge enum to a glyph + label. This is the ONLY
// place that owns opening-badge presentation — a new badge value gets a new
// case here, nowhere else. Green (✅) is reserved for the real "clean"
// verdict; "done" is a neutral outcome (⚪), not a success signal.
func openingBadgeLabel(b spiceboxv1alpha1.OpeningBadge) string {
	switch b {
	case spiceboxv1alpha1.OpeningBadgeInProgress:
		return "🔄 In progress"
	case spiceboxv1alpha1.OpeningBadgeClean:
		return "✅ Clean"
	case spiceboxv1alpha1.OpeningBadgeProblemsFound:
		return "⚠️ Problems found"
	case spiceboxv1alpha1.OpeningBadgeCouldNotFinish:
		return "🚧 Couldn't finish"
	case spiceboxv1alpha1.OpeningBadgeUnfinished:
		return "🚫 Didn't finish"
	case spiceboxv1alpha1.OpeningBadgeDone:
		return "⚪ Done"
	default:
		return ""
	}
}

// renderOpeningText composes the badge label, the opening line, the
// supporting body, and an optional deep link (Slack mrkdwn) into the single
// string used both for the message's block section and its text fallback.
func renderOpeningText(content channelkinds.OpeningMessageContent) string {
	var sb strings.Builder
	if label := openingBadgeLabel(content.Badge); label != "" {
		sb.WriteString(label)
		sb.WriteString("  ")
	}
	sb.WriteString(content.OpeningText)
	if content.Body != "" {
		sb.WriteString("\n\n")
		sb.WriteString(content.Body)
	}
	if content.Link != "" {
		sb.WriteString("\n<")
		sb.WriteString(content.Link)
		sb.WriteString("|View report>")
	}
	return sb.String()
}

// EditOpeningMessage implements channelkinds.OpeningMessageEditor by
// chat.update-ing the message identified by content.Ref in place. A missing
// channel_id/ts means there is no message to edit at all — that is an error,
// not a silent no-op. A chat.update failure is logged with enough structured
// context to locate the session/message and returned wrapped, per the
// no-silent-errors rule.
func (s *slackSender) EditOpeningMessage(ctx context.Context, sess channelkinds.SessionInfo, content channelkinds.OpeningMessageContent) error {
	if content.Ref.ChannelID == "" || content.Ref.TS == "" {
		return fmt.Errorf("slack EditOpeningMessage: missing channel_id/ts")
	}
	text := renderOpeningText(content)
	block := slackapi.NewSectionBlock(slackapi.NewTextBlockObject("mrkdwn", text, false, false), nil, nil)
	if _, _, _, err := s.client.UpdateMessageContext(ctx, content.Ref.ChannelID, content.Ref.TS,
		slackapi.MsgOptionText(text, false),
		slackapi.MsgOptionBlocks(block),
	); err != nil {
		log.FromContext(ctx).Info("slack: EditOpeningMessage chat.update failed",
			"session", sess.Namespace+"/"+sess.Name, "channelID", content.Ref.ChannelID, "ts", content.Ref.TS, "err", err.Error())
		return fmt.Errorf("slack chat.update opening message: %w", err)
	}
	return nil
}
