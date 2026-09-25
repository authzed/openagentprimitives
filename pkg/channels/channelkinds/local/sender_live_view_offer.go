package local

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
// for the TUI. When a live-view is ready, it mints a signed webd deep-link
// and emits it as a MsgLiveViewOffer timeline note ("View in browser: <url>").
// When browser links are unavailable (nil minter, or an empty base URL), it
// surfaces a visible MsgSendError and returns an error rather than dropping the
// offer silently — cmd/oap's one-time startup notice is a first-line signal, not
// a substitute for surfacing the failure of a specific offer.
type liveViewOfferSender struct {
	sink      *inertSink
	minter    channelkinds.ArtifactViewMinter
	principal identity.Principal
}

func (s *liveViewOfferSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	var p channelevents.LiveViewOfferPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: err.Error(), At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local live_view_offer sender: parse payload: %w", err)
	}
	if p.ArtifactID == "" {
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "payload missing artifactId", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local live_view_offer sender: payload missing artifactId")
	}

	if s.minter == nil {
		// A nil minter means browser view links are not configured. cmd/oap
		// prints a one-time startup notice, but that is a first-line signal,
		// not a substitute: the agent explicitly offered the user a live view,
		// so surface the failure loudly at that moment (INFO log + a visible
		// send_error) instead of dropping it with only a V(1) log. See
		// AGENTS.md "Never silently drop errors".
		logger.Info("local live_view_offer: minter not configured; cannot deliver browser link", "artifactID", p.ArtifactID)
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "live view unavailable: this cluster is not configured to mint browser view links", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local live_view_offer sender: minter not configured")
	}

	sessionRef := sess.Namespace + "/" + sess.Name
	url, err := s.minter.MintArtifactViewLink(p.ArtifactID, sessionRef, s.principal, "")
	if err != nil {
		logger.Info("local live_view_offer: mint failed", "artifactID", p.ArtifactID, "err", err.Error())
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "mint artifact view link: " + err.Error(), At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local live_view_offer sender: mint: %w", err)
	}
	if url == "" {
		// An empty URL means the minter is wired but webd's external base URL
		// is not populated yet (the ConfigMap is unset). No startup notice
		// covers this runtime condition, so surface it loudly rather than
		// skipping silently — the user was promised a live view and must be
		// told why it's missing.
		logger.Info("local live_view_offer: webd base URL empty; cannot deliver browser link", "artifactID", p.ArtifactID)
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "live view unavailable: this cluster's external URL is not configured yet", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local live_view_offer sender: webd base URL empty")
	}

	s.sink.Emit(MsgLiveViewOffer{Session: refOf(sess), URL: url})
	logger.Info("local live_view_offer: emitted browser link", "artifactID", p.ArtifactID)
	return channelkinds.SubChannelSendResult{}, nil
}
