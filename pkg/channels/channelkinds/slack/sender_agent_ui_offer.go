// pkg/channels/channelkinds/slack/sender_agent_ui_offer.go
//
// agentUIOfferSender handles the "agent_ui_offer" sub-channel: it posts a
// Slack message with a URL button pointing at the agent-UI shell page
// ("<webd-base>/sessions?session=<ns>/<name>"). Same durable-link shape as
// session_view_offer (see that file's doc comment for why the URL bakes in
// at post time rather than being minted per click), rendered through the
// shared buildURLOfferBlocks.
//
// Where this sender diverges from session_view_offer is failure handling.
// session_view_offer's offer is a secondary anchor posted alongside the
// agent's real reply, so a missing minter is a quiet log-and-skip. This
// offer is the tool result's only delivery: the agent already told the model
// (and, through it, the user) that a dashboard link was sent, so dropping it
// quietly would reproduce the "delivered, nothing in channel" failure
// AGENTS.md's no-silent-errors rule exists to prevent. When a link can't be
// produced, Send logs, posts a plain-text notice in place of the button, and
// still returns an error — the notice is the only user-visible half, and if
// posting the notice also fails there is nothing further to try.
package slack

import (
	"context"
	"encoding/json"
	"fmt"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// agentUIOfferActionID is the action_id Slack requires on the button.
// Nothing consumes the click server-side — the button carries a URL, so
// Slack opens it directly.
const agentUIOfferActionID = "open_agent_ui"

// agentUIOfferNoticeText replaces the button when a link can't be produced
// (no minter wired, or a minted URL that came back empty). Deliberately free
// of any configuration key name or Go identifier: it is the only
// user-visible half of a promise the agent already made, so it has to read
// like a message from the agent, not a log line.
const agentUIOfferNoticeText = "I couldn't produce a link to the dashboard just now."

// agentUIOfferSender implements channelkinds.Sender for the agent_ui_offer
// sub-channel on Slack.
type agentUIOfferSender struct {
	client slackClient
	// minter composes the shell page URL. Declared as the interface type so
	// the zero value is a true nil interface.
	minter channelkinds.AgentUIMinter
}

func newAgentUIOfferSender(deps channelkinds.Deps) *agentUIOfferSender {
	return &agentUIOfferSender{
		client: newSlackAPIClient(deps.Secret),
		minter: deps.AgentUIMinter,
	}
}

// Send conforms to channelkinds.Sender.
func (s *agentUIOfferSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	if s.client == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("agent_ui_offer sender: slack client unconfigured (Secret missing bot token)")
	}

	var p channelevents.AgentUIOfferPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("agent_ui_offer sender: parse payload: %w", err)
	}
	if p.SessionRef == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("agent_ui_offer sender: payload missing sessionRef")
	}

	channelID, threadTS := resolveChannelAndThread(sess)
	if channelID == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("agent_ui_offer sender: session has no channel_id")
	}

	if s.minter == nil {
		logger.Info("agent_ui_offer: webd not configured; posting failure notice")
		if postErr := s.postNotice(ctx, channelID, threadTS); postErr != nil {
			logger.Info("agent_ui_offer: failed to post failure notice", "err", postErr.Error())
		}
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("agent_ui_offer sender: webd not configured")
	}

	url, err := s.minter.MintAgentUILink(p.SessionRef)
	if err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("agent_ui_offer sender: mint link: %w", err)
	}
	if url == "" {
		logger.Info("agent_ui_offer: webd URL not yet available; posting failure notice")
		if postErr := s.postNotice(ctx, channelID, threadTS); postErr != nil {
			logger.Info("agent_ui_offer: failed to post failure notice", "err", postErr.Error())
		}
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("agent_ui_offer sender: webd URL not yet available")
	}

	blocks := buildURLOfferBlocks(
		"agent_ui_offer_actions",
		agentUIOfferActionID,
		"*This conversation has a live dashboard* — open it in your browser.",
		"🖥️ Open dashboard",
		url,
	)
	opts := []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(blocks...),
		slackapi.MsgOptionText("Dashboard ready", false),
	}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, _, err := s.client.PostMessageContext(ctx, channelID, opts...); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("agent_ui_offer sender: PostMessage: %w", err)
	}
	logger.Info("agent_ui_offer: posted dashboard button", "sessionRef", p.SessionRef)
	return channelkinds.SubChannelSendResult{}, nil
}

// postNotice posts agentUIOfferNoticeText as a plain-text message in place
// of the button. Its own error is logged by the caller rather than
// propagated: the caller already has a distinct error to return for why the
// link couldn't be produced, and this post is a best-effort second attempt
// at surfacing that to the user, not the primary failure.
func (s *agentUIOfferSender) postNotice(ctx context.Context, channelID, threadTS string) error {
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(agentUIOfferNoticeText, false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	_, _, err := s.client.PostMessageContext(ctx, channelID, opts...)
	return err
}
