// pkg/channels/channelkinds/slack/sender_user_echo.go
//
// userEchoSender handles the "user_echo" sub-channel: the outbound mirror of
// a view-originated message (an artifact-view/webui-typed message that
// routed to the session) back into the session's originating Slack thread,
// so participants there see what was actually said instead of the agent
// apparently answering a question nobody asked. See
// channelevents.KindUserEcho / channelevents.UserEchoPayload and the
// publisher, pkg/channels/channelsd/pipeline/view_message.go.
package slack

import (
	"context"
	"encoding/json"
	"fmt"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
)

// userEchoSender implements channelkinds.Sender for the "user_echo"
// sub-channel on Slack.
type userEchoSender struct {
	client slackClient
	// k8s backs the mentionForSubject lookup. Nil (K8sClient not wired) is
	// tolerated — the sender degrades to the escaped-name fallback rather
	// than failing the mirror.
	k8s client.Client
}

func newUserEchoSender(deps channelkinds.Deps) *userEchoSender {
	return &userEchoSender{
		client: newSlackAPIClient(deps.Secret),
		k8s:    deps.K8sClient,
	}
}

// Send conforms to channelkinds.Sender.
func (s *userEchoSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	if s.client == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("user_echo sender: slack client unconfigured (Secret missing bot token)")
	}
	if sess.Channel == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("user_echo sender: session has no channel binding")
	}

	var p channelevents.UserEchoPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("user_echo sender: parse payload: %w", err)
	}

	channelID := sess.Channel.External["channel_id"]
	if channelID == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("user_echo sender: external.channel_id missing on session.spec.channel")
	}
	threadTS := effectiveOutboundThreadTS(sess)

	text := "💬 " + s.resolveMention(ctx, sess, p.Author)
	if via := viewurn.Describe(p.Via); via != "" {
		text += ", via " + via
	}
	text += ": " + escapeSlackText(p.Text)

	opts := []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, _, err := s.client.PostMessageContext(ctx, channelID, opts...); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("user_echo sender: PostMessage: %w", err)
	}
	logger.Info("user_echo: posted mirrored view message", "channelID", channelID, "via", p.Via)
	return channelkinds.SubChannelSendResult{}, nil
}

// resolveMention returns the Slack markup to render for the message's
// author: a real "<@Uxxxx>" mention when the K8s-witnessed reverse
// directory (UserIdentity.status.channelIdentities) has a slack identity
// for this author in THIS session's workspace (team_id), else an escaped
// fallback label. The @mention is NEVER derived from wire display text —
// only from the server-witnessed channelIdentities keyed by the canonical
// subject re-derived from Author here (mirroring SessionOwner's pattern in
// schema_fragment.go: AllowSynthetic because this is a best-effort render,
// not an authorization decision — a guest/no-email author still gets a
// lookup attempt rather than an error).
func (s *userEchoSender) resolveMention(ctx context.Context, sess channelkinds.SessionInfo, author channelevents.ExternalIdentity) string {
	if s.k8s != nil {
		subject, err := identity.FromExternal(identity.Kind(author.Kind), identity.TeamScope(author.TeamScope),
			identity.RawExternalID(author.ExternalID), identity.Email(author.Email)).
			AllowSynthetic().Subject()
		if err == nil {
			teamID := ""
			if sess.Channel != nil {
				teamID = sess.Channel.External["team_id"]
			}
			// identity boundary: mentionForSubject takes a string subject for the Slack mention lookup.
			if slackID, ok := mentionForSubject(ctx, s.k8s, subject.String(), teamID); ok {
				return "<@" + slackID + ">"
			}
		}
	}
	return escapeSlackText(fallbackAuthorName(author))
}

// fallbackAuthorName picks the best available human-readable label for an
// author with no resolvable slack mention: their verified email, else their
// channel-native external id, else a generic placeholder. Never empty.
func fallbackAuthorName(author channelevents.ExternalIdentity) string {
	if author.Email != "" {
		return author.Email.String()
	}
	if author.ExternalID != "" {
		return author.ExternalID.String()
	}
	return "someone"
}
