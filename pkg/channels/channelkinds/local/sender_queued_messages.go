package local

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// queuedMessagesSender is the host-side "queued_messages" sub-channel
// sender. Today it only renders the outcome of a mid-turn interrupt
// request (KindInterruptApplied); a future ack kind (P2's queue+confirm
// flow) will extend the switch below. An envelope kind this sender
// doesn't yet know about is a silent no-op, not a surfaced send_error —
// unlike the other sub-channel senders, which treat any unrecognized
// kind as a hard failure.
type queuedMessagesSender struct {
	sink *inertSink
}

func (s *queuedMessagesSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindInterruptApplied:
		var pl channelevents.InterruptAppliedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: err.Error(), At: timeNowUTC()})
			return channelkinds.SubChannelSendResult{}, fmt.Errorf("local queued_messages sender (%s): %w", env.Kind, err)
		}
		s.sink.Emit(MsgInterruptApplied{
			Session:   refOf(sess),
			RequestID: pl.RequestID,
			Outcome:   pl.Outcome,
			Reason:    pl.Reason,
		})
		return channelkinds.SubChannelSendResult{}, nil

	default:
		// Unrecognized kind for this sub-channel — silently no-op (see the
		// type doc), but log it so a future mis-wired kind is greppable
		// instead of vanishing.
		log.FromContext(ctx).Info("local queued_messages sender: unexpected envelope kind, no-op",
			"kind", env.Kind, "session", sess.Namespace+"/"+sess.Name)
		return channelkinds.SubChannelSendResult{}, nil
	}
}
