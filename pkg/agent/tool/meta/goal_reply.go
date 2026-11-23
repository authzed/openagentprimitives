package meta

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
)

// GoalReplyAcceptor waits for a durable delivery receipt from the operator.
// Retrying the same payload reconciles the existing delivery instead of sending
// another reply; cancellation stops waiting without claiming delivery failed.
func GoalReplyAcceptor(call GoalsCaller) func(context.Context, channelevents.OutboundUserMessagePayload) (delivery.Receipt, error) {
	return func(ctx context.Context, payload channelevents.OutboundUserMessagePayload) (delivery.Receipt, error) {
		if call == nil {
			return delivery.Receipt{}, fmt.Errorf("goal reply acceptance unavailable")
		}
		for {
			response, err := call(ctx, goals.Request{Operation: "prepare_reply", Reply: &payload})
			if err != nil {
				return delivery.Receipt{}, err
			}
			if response.Run == nil || response.Run.Reply == nil || !reflect.DeepEqual(response.Run.Reply.Intent.Payload, payload) {
				return delivery.Receipt{}, delivery.ErrConflict
			}
			reply := response.Run.Reply
			if reply.State == "accepted" && reply.Receipt != nil {
				if err := reply.Receipt.Validate(reply.Intent); err != nil {
					return delivery.Receipt{}, err
				}
				return *reply.Receipt, nil
			}
			if reply.State == "absent" {
				return delivery.Receipt{}, fmt.Errorf("reply was not accepted before execution stopped")
			}
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return delivery.Receipt{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
}
