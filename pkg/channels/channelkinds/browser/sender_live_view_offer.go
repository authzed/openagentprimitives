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

// liveViewOfferSender is the host-side "live_view_offer" sub-channel sender
// for the browser page. When a live-view is ready, it mints a signed webd
// deep-link and emits it as a MsgLiveViewOffer timeline note ("View in
// browser: <url>"). A nil minter (webd not configured) results in a silent
// skip — the startup notice already informed the user that browser links
// are unavailable.
type liveViewOfferSender struct {
	sink      EventSink
	minter    channelkinds.ArtifactViewMinter
	principal identity.Principal
}

func (s *liveViewOfferSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	var p channelevents.LiveViewOfferPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: err.Error(), At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser live_view_offer sender: parse payload: %w", err)
	}
	if p.ArtifactID == "" {
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "payload missing artifactId", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser live_view_offer sender: payload missing artifactId")
	}

	if s.minter == nil {
		// A nil minter means this server was never wired to mint browser view
		// links — a misconfiguration, not a feature toggle. The agent
		// explicitly offered the user a live view and the user is waiting for
		// it, so surface the failure loudly (operator log + a visible
		// send_error in the chat) instead of dropping it silently. A silent
		// skip here is exactly the bug that swallowed a real session's
		// artifact_offer_view — see AGENTS.md "Never silently drop errors".
		logger.Info("browser live_view_offer: minter not configured; cannot deliver browser link", "artifactID", p.ArtifactID)
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "live view unavailable: this server is not configured to mint browser view links", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser live_view_offer sender: minter not configured")
	}

	sessionRef := sess.Namespace + "/" + sess.Name
	url, err := s.minter.MintArtifactViewLink(p.ArtifactID, sessionRef, s.principal, "")
	if err != nil {
		logger.Info("browser live_view_offer: mint failed", "artifactID", p.ArtifactID, "err", err.Error())
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "mint artifact view link: " + err.Error(), At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser live_view_offer sender: mint: %w", err)
	}
	if url == "" {
		// An empty URL means the minter is wired but webd's external base URL
		// is not populated yet (the ConfigMap is unset). The offer still can't
		// reach the browser, so surface it loudly rather than skipping silently
		// — the user was promised a live view and must be told why it's missing.
		logger.Info("browser live_view_offer: webd base URL empty; cannot deliver browser link", "artifactID", p.ArtifactID)
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "live view unavailable: this server's external URL is not configured yet", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser live_view_offer sender: webd base URL empty")
	}

	s.sink.Emit(MsgLiveViewOffer{Session: refOf(sess), URL: url})
	logger.Info("browser live_view_offer: emitted browser link", "artifactID", p.ArtifactID)
	return channelkinds.SubChannelSendResult{}, nil
}
