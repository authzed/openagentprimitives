package local

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// permissionRequestSender is the host-side "permission_request"
// sub-channel sender: KindPermissionRequest (multiplayer session-join)
// and KindPermissionDecisionApplied. v1 local sessions are single-user,
// so these are rendered as informational timeline notes by the model,
// not modals.
type permissionRequestSender struct {
	sink *inertSink
}

func (s *permissionRequestSender) emitErr(sess channelkinds.SessionInfo, k channelevents.Kind, err error) (channelkinds.SubChannelSendResult, error) {
	s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: k, Err: err.Error(), At: timeNowUTC()})
	return channelkinds.SubChannelSendResult{}, fmt.Errorf("local permission_request sender (%s): %w", k, err)
}

func (s *permissionRequestSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindPermissionRequest:
		var pl channelevents.PermissionRequestPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		s.sink.Emit(MsgPermissionRequest{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindPermissionDecisionApplied:
		var pl channelevents.PermissionDecisionAppliedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		s.sink.Emit(MsgPermissionDecisionApplied{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	default:
		return s.emitErr(sess, env.Kind, fmt.Errorf("unsupported envelope kind for the permission_request sender"))
	}
}
