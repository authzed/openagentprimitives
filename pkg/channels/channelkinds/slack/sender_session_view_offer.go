// pkg/channels/channelkinds/slack/sender_session_view_offer.go
//
// sessionViewOfferSender handles the "session_view_offer" sub-channel. On
// Send it posts a Slack message with a URL button pointing at the browser
// session-view page ("<webd-base>/session-view/{ns}/{name}"). Unlike
// live_view_offer's button (which carries no URL and is minted fresh per
// click, because a signed link would go stale), the session-view link is a
// durable plain-path link with no TTL and no embedded signature —
// authorization happens at open time via the page's own CheckInteract, so
// baking the URL in at post time is safe.
package slack

import (
	"context"
	"encoding/json"
	"fmt"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// sessionViewOfferActionID is the action_id Slack requires on the "Open
// interactive view" button. Nothing consumes this click server-side — the
// button carries a URL, so Slack opens it directly in the browser.
const sessionViewOfferActionID = "open_session_view"

// sessionViewOfferSender implements channelkinds.Sender for the
// "session_view_offer" sub-channel on Slack.
type sessionViewOfferSender struct {
	client slackClient
	// minter gates whether the offer is posted at all: nil means webd is not
	// configured on this install, so the link could never resolve. Declared
	// as the interface type so the zero value is a true nil interface.
	minter channelkinds.SessionViewMinter
}

func newSessionViewOfferSender(deps channelkinds.Deps) *sessionViewOfferSender {
	return &sessionViewOfferSender{
		client: newSlackAPIClient(deps.Secret),
		minter: deps.SessionViewMinter,
	}
}

// Send conforms to channelkinds.Sender.
func (s *sessionViewOfferSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	if s.client == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("session_view_offer sender: slack client unconfigured (Secret missing bot token)")
	}

	var p channelevents.SessionViewOfferPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("session_view_offer sender: parse payload: %w", err)
	}
	if p.SessionRef == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("session_view_offer sender: payload missing sessionRef")
	}

	// Gate: only offer the link when webd is configured.
	if s.minter == nil {
		logger.Info("session-view offer: webd not configured; skipping")
		return channelkinds.SubChannelSendResult{}, nil
	}

	channelID := ""
	threadTS := ""
	if sess.Channel != nil {
		channelID = sess.Channel.External["channel_id"]
		threadTS = effectiveOutboundThreadTS(sess)
	}
	if channelID == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("session_view_offer sender: session has no channel_id")
	}

	// No per-viewer identity is available in this proactive path (unlike
	// live_view_offer's click handler, which mints fresh per clicker): the
	// session-view page enforces its own CheckInteract at open time, so the
	// subject/backLink args below are accepted for interface parity only and
	// have no bearing on who may actually open the link.
	url, err := s.minter.MintSessionViewLink(p.SessionRef, identity.Principal{}, "")
	if err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("session_view_offer sender: mint link: %w", err)
	}
	if url == "" {
		logger.Info("session-view offer: webd URL not yet available; skipping")
		return channelkinds.SubChannelSendResult{}, nil
	}

	blocks := buildSessionViewOfferBlocks(url)
	opts := []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(blocks...),
		slackapi.MsgOptionText("Interactive view ready", false),
	}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, _, err := s.client.PostMessageContext(ctx, channelID, opts...); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("session_view_offer sender: PostMessage: %w", err)
	}
	logger.Info("session_view_offer: posted interactive-view anchor", "sessionRef", p.SessionRef)
	return channelkinds.SubChannelSendResult{}, nil
}

// buildSessionViewOfferBlocks returns the Block Kit blocks for the
// session-view offer: a section prompt + a URL button opening the
// interactive view page directly. No click-time interaction is involved —
// the link is a durable plain path, so it can be baked into the button at
// post time (unlike live_view_offer's interaction button, minted fresh per
// click). Delegates to the shared buildURLOfferBlocks (url_offer_blocks.go),
// which agent_ui_offer's sender also uses — the two offers share this exact
// "section + one primary URL button" shape.
func buildSessionViewOfferBlocks(url string) []slackapi.Block {
	return buildURLOfferBlocks(
		"session_view_offer_actions",
		sessionViewOfferActionID,
		"*Interactive view ready* — open it in your browser.",
		"🖥️ Open interactive view",
		url,
	)
}
