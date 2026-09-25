// pkg/channels/channelkinds/slack/sender_live_view_offer.go
//
// liveViewOfferSender handles the "live_view_offer" sub-channel. On Send it
// posts a Slack block_actions button "👁 View live" whose value encodes the
// artifact identity + channel/thread. The button carries NO URL: a URL button
// would freeze a 30-minute signed link at post time, which 403s once the TTL
// lapses. Instead the listener mints a FRESH link on every click (see
// handleLiveViewClick), so the button works for the life of the session.
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

// liveViewActionID is the action_id on the "View live" interaction button.
// Shared by this sender (producer) and the listener (consumer).
const liveViewActionID = "view_live_artifact"

// liveViewButtonValue is the JSON encoded into the "View live" button's value.
// Short keys keep it well under Slack's 2000-char button-value limit.
type liveViewButtonValue struct {
	// V is the "live_view" discriminator; anything else is not this button.
	V string `json:"v"`
	// A is the artifact the click should mint a fresh view link for.
	A string `json:"a"`
	// S is the session this artifact belongs to, as "namespace/name".
	S string `json:"s"`
	// C is where the offer was posted, so the reply lands on the same surface.
	C string `json:"c"`
	// T is the enclosing thread; empty means the offer was posted top-level.
	T string `json:"t"`
}

// liveViewOfferSender implements channelkinds.Sender for the
// "live_view_offer" sub-channel on Slack.
type liveViewOfferSender struct {
	client slackClient
	// minter gates whether the offer is posted at all: nil means webd is not
	// configured on this install, so a "View live" button could never resolve.
	// Declared as the interface type so the zero value is a true nil interface.
	// The sender does NOT mint — the listener mints fresh at click time.
	minter channelkinds.ArtifactViewMinter
}

func newLiveViewOfferSender(deps channelkinds.Deps) *liveViewOfferSender {
	return &liveViewOfferSender{
		client: newSlackAPIClient(deps.Secret),
		minter: deps.ArtifactViewMinter,
	}
}

// Send conforms to channelkinds.Sender.
func (s *liveViewOfferSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	if s.client == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("live_view_offer sender: slack client unconfigured (Secret missing bot token)")
	}

	var p channelevents.LiveViewOfferPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("live_view_offer sender: parse payload: %w", err)
	}
	if p.ArtifactID == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("live_view_offer sender: payload missing artifactId")
	}

	// Gate: only offer the button when webd is configured. We do NOT gate on
	// the live webd base URL — a fresh mint at click time may succeed even if
	// the URL ConfigMap was empty when the offer was posted.
	//
	// A nil minter means this install was never wired to mint artifact view
	// links, so a "View live" button could never resolve and posting one would
	// hand the user a control that 403s. Posting nothing is therefore right —
	// but it is a misconfiguration, not a feature toggle, and the agent
	// explicitly offered the user a live view they are now waiting for. Return
	// the failure so the relay logs it against this session; a bare nil left
	// the whole path with no trace to grep for, which is how a real session's
	// artifact_offer_view came to report an offer nobody could see.
	if s.minter == nil {
		logger.Info("live_view_offer: webd not configured; cannot post an offer that could resolve", "artifactID", p.ArtifactID)
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("live_view_offer sender: artifact view minter not configured")
	}

	channelID := ""
	threadTS := ""
	if sess.Channel != nil {
		channelID = sess.Channel.External["channel_id"]
		threadTS = effectiveOutboundThreadTS(sess)
	}
	if channelID == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("live_view_offer sender: session has no channel_id")
	}
	sessionRef := sess.Namespace + "/" + sess.Name

	blocks, err := buildLiveViewOfferBlocks(p.ArtifactID, sessionRef, channelID, threadTS)
	if err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("live_view_offer sender: build blocks: %w", err)
	}
	opts := []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(blocks...),
		slackapi.MsgOptionText("Live view ready", false),
	}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, _, err := s.client.PostMessageContext(ctx, channelID, opts...); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("live_view_offer sender: PostMessage: %w", err)
	}
	logger.Info("live_view_offer: posted interaction button", "artifactID", p.ArtifactID)
	return channelkinds.SubChannelSendResult{}, nil
}

// buildLiveViewOpenBlocks returns the Block Kit blocks for the ephemeral
// reply the listener posts on a "View live" click: a short note + a URL
// button that opens the freshly-minted live-view link in the browser.
func buildLiveViewOpenBlocks(url string) []slackapi.Block {
	btn := slackapi.NewButtonBlockElement(
		"open_live_artifact",
		"",
		slackapi.NewTextBlockObject(slackapi.PlainTextType, "🔗 Open live view", false, false),
	).WithURL(url).WithStyle(slackapi.StylePrimary)

	return []slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, "Your live view is ready.", false, false),
			nil, nil,
		),
		slackapi.NewActionBlock("live_view_open_actions", btn),
	}
}

// buildLiveViewOfferBlocks returns the Block Kit blocks for the live-view
// offer: a section prompt + an actions block with an INTERACTION button (no
// URL) whose value carries the artifact identity + channel/thread.
func buildLiveViewOfferBlocks(artifactID, sessionRef, channelID, threadTS string) ([]slackapi.Block, error) {
	val, err := json.Marshal(liveViewButtonValue{
		V: "live_view", A: artifactID, S: sessionRef, C: channelID, T: threadTS,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal live-view button value: %w", err)
	}
	btn := slackapi.NewButtonBlockElement(
		liveViewActionID,
		string(val),
		slackapi.NewTextBlockObject(slackapi.PlainTextType, "👁 View live", false, false),
	).WithStyle(slackapi.StylePrimary)

	return []slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, "*Live view ready* — open the artifact in your browser.", false, false),
			nil, nil,
		),
		slackapi.NewActionBlock("live_view_offer_actions", btn),
	}, nil
}
