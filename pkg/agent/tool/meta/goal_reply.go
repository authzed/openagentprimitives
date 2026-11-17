package meta

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
)

// ErrGoalReplyPublishFailed: this call's reply was handed off earlier but the
// runner could not publish it, so nothing was posted and the run records it
// as publish_failed. It is terminal; the call does not publish again.
var ErrGoalReplyPublishFailed = errors.New("this reply was handed off earlier but could not be published; nothing was posted and the run records it as publish_failed")

// GoalReplyPublishFailed reports to the operator that a handed-off reply was
// not published, moving its record from unconfirmed to publish_failed.
func GoalReplyPublishFailed(call GoalsCaller) func(context.Context, channelevents.OutboundUserMessagePayload) error {
	return func(ctx context.Context, payload channelevents.OutboundUserMessagePayload) error {
		if call == nil {
			return fmt.Errorf("goal reply failure recording unavailable")
		}
		response, err := call(ctx, goals.Request{Operation: "reply_publish_failed", Reply: &payload})
		if err != nil {
			return err
		}
		if response.Run == nil || response.Run.Reply == nil || response.Run.Reply.State != goals.ReplyPublishFailed {
			return delivery.ErrConflict
		}
		return nil
	}
}

// GoalReplyAcceptor waits for a durable delivery receipt from the operator.
// Retrying the same payload reconciles the existing delivery instead of sending
// another reply; cancellation stops waiting without claiming delivery failed.
//
// A nil receipt with a nil error is a hand-off: a group run on a transport
// without a delivery receiver, whose reply the operator recorded as
// unconfirmed. The caller publishes it once through the ordinary outbound
// relay and must not present it as a confirmed delivery.
func GoalReplyAcceptor(call GoalsCaller) func(context.Context, channelevents.OutboundUserMessagePayload) (*delivery.Receipt, error) {
	return func(ctx context.Context, payload channelevents.OutboundUserMessagePayload) (*delivery.Receipt, error) {
		if call == nil {
			return nil, fmt.Errorf("goal reply acceptance unavailable")
		}
		for {
			response, err := call(ctx, goals.Request{Operation: "prepare_reply", Reply: &payload})
			if err != nil {
				return nil, err
			}
			if response.Run == nil || response.Run.Reply == nil || !reflect.DeepEqual(response.Run.Reply.Intent.Payload, payload) {
				return nil, delivery.ErrConflict
			}
			reply := response.Run.Reply
			switch reply.State {
			case goals.ReplyUnconfirmed:
				if reply.Receipt != nil {
					return nil, delivery.ErrConflict
				}
				return nil, nil
			case goals.ReplyPublishFailed:
				return nil, ErrGoalReplyPublishFailed
			case goals.ReplyAccepted:
				if reply.Receipt != nil {
					if err := reply.Receipt.Validate(reply.Intent); err != nil {
						return nil, err
					}
					return reply.Receipt, nil
				}
			case goals.ReplyAbsent:
				return nil, fmt.Errorf("reply was not accepted before execution stopped")
			}
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
}
