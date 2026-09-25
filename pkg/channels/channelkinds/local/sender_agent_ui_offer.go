package local

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// agentUIOfferSender is the host-side agent_ui_offer sub-channel sender for
// the TUI. It mints EAGERLY and treats a nil minter, a mint error, or an
// empty URL as a LOUD failure (a visible MsgSendError plus a returned error)
// rather than a silent skip: the agent explicitly offered the user this
// session's dashboard and the user is waiting for the link. Same reasoning
// as this package's session-view and live-view offer senders.
type agentUIOfferSender struct {
	sink   *inertSink
	minter channelkinds.AgentUIMinter
}

func (s *agentUIOfferSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	var p channelevents.AgentUIOfferPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: err.Error(), At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local agent_ui_offer sender: parse payload: %w", err)
	}
	if p.SessionRef == "" {
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "payload missing sessionRef", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local agent_ui_offer sender: payload missing sessionRef")
	}

	if s.minter == nil {
		// A nil minter means the agent-UI shell page is not configured for this
		// cluster. Surface this loudly at the moment the agent explicitly
		// offered the dashboard, instead of dropping it with only a V(1) log.
		// See AGENTS.md "Never silently drop errors".
		logger.Info("local agent_ui_offer: minter not configured; cannot deliver dashboard link", "sessionRef", p.SessionRef)
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "dashboard link unavailable: this cluster is not configured to mint agent-UI links", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local agent_ui_offer sender: minter not configured")
	}

	url, err := s.minter.MintAgentUILink(p.SessionRef)
	if err != nil {
		logger.Info("local agent_ui_offer: mint failed", "sessionRef", p.SessionRef, "err", err.Error())
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "mint agent UI link: " + err.Error(), At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local agent_ui_offer sender: mint: %w", err)
	}
	if url == "" {
		// An empty URL means the minter is wired but webd's external base URL
		// is not populated yet. No startup notice covers this runtime
		// condition, so surface it loudly rather than skipping silently.
		logger.Info("local agent_ui_offer: webd base URL empty; cannot deliver dashboard link", "sessionRef", p.SessionRef)
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: "dashboard link unavailable: this cluster's external URL is not configured yet", At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("local agent_ui_offer sender: webd base URL empty")
	}

	s.sink.Emit(MsgAgentUIOffer{Session: refOf(sess), URL: url})
	logger.Info("local agent_ui_offer: emitted dashboard link", "sessionRef", p.SessionRef)
	return channelkinds.SubChannelSendResult{}, nil
}
