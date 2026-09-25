package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// sessionViewOfferSender is the host-side "session_view_offer" sub-channel
// sender for the browser page. Unlike Slack's — where the offer is a secondary
// anchor beside the primary chat message, so a missing webd config skips
// quietly — this is the page's ONLY delivery path for the escalation anchor. It
// therefore mints EAGERLY and treats a nil minter or an empty URL as a LOUD
// failure (a visible MsgSendError): the agent explicitly escalated an
// interactive widget and the user is waiting to see it.
//
// It emits the same MsgLiveViewOffer event live_view_offer uses — both are
// "here's a link" notes of identical shape — so this link needs no additional
// frontend wiring.
type sessionViewOfferSender struct {
	sink      EventSink
	minter    channelkinds.SessionViewMinter
	principal identity.Principal
}

func (s *sessionViewOfferSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	var p channelevents.SessionViewOfferPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: err.Error(), At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser session_view_offer sender: parse payload: %w", err)
	}
	if p.SessionRef == "" {
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "payload missing sessionRef", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser session_view_offer sender: payload missing sessionRef")
	}

	if s.minter == nil {
		// A nil minter is a misconfiguration, not a feature toggle: the agent
		// explicitly offered the user an interactive view, so fail loudly.
		logger.Info("browser session_view_offer: minter not configured; cannot deliver browser link", "sessionRef", p.SessionRef)
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "interactive view unavailable: this server is not configured to mint browser view links", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser session_view_offer sender: minter not configured")
	}

	// No per-viewer identity is meaningful: the link carries no signed
	// capability, so s.principal has no bearing on the composed URL. Passed
	// anyway to keep this sender symmetric with liveViewOfferSender.
	url, err := s.minter.MintSessionViewLink(p.SessionRef, s.principal, "")
	if err != nil {
		logger.Info("browser session_view_offer: mint failed", "sessionRef", p.SessionRef, "err", err.Error())
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "mint session view link: " + err.Error(), At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser session_view_offer sender: mint: %w", err)
	}
	if url == "" {
		// An empty URL means the minter is wired but webd's external base URL
		// is not populated yet. Surface it loudly rather than skipping
		// silently — the user was promised an interactive view.
		logger.Info("browser session_view_offer: webd base URL empty; cannot deliver browser link", "sessionRef", p.SessionRef)
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "interactive view unavailable: this server's external URL is not configured yet", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser session_view_offer sender: webd base URL empty")
	}

	s.sink.Emit(MsgLiveViewOffer{Session: refOf(sess), URL: url})
	logger.Info("browser session_view_offer: emitted browser link", "sessionRef", p.SessionRef)
	return channelkinds.SubChannelSendResult{}, nil
}
