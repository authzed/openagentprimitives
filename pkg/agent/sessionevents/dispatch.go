package sessionevents

import (
	"context"
	"fmt"
	"time"
)

// LaunchConsumer durably materializes a session request using Admission.LaunchID
// as its idempotency key. A successful return means the request is retained;
// actual activation must still check current authority, deadlines and capacity.
// This contract lets goals and other asynchronous producers share the outbox.
type LaunchConsumer interface {
	Materialize(context.Context, Launch) error
}

type Dispatcher struct {
	Triggers *Triggers
	Consumer LaunchConsumer
}

// Drain leaves a launch pending on failure. If materialization committed but its
// acknowledgment was lost, the next drain repeats the same idempotent request.
// Polling workers need no permission to create an alternative launch identity.
func (d *Dispatcher) Drain(ctx context.Context, now time.Time, limit int) (int, error) {
	if d == nil || d.Triggers == nil || d.Triggers.Store == nil || d.Consumer == nil {
		return 0, ErrDenied
	}
	launches, err := d.Triggers.Store.Pending(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	dispatched := 0
	for _, launch := range launches {
		if err = ctx.Err(); err != nil {
			return dispatched, err
		}
		if err = d.Triggers.CheckLaunch(ctx, launch, now); err != nil {
			return dispatched, fmt.Errorf("authorize event launch %s: %w", launch.Admission.LaunchID, err)
		}
		if err = d.Consumer.Materialize(ctx, launch); err != nil {
			return dispatched, fmt.Errorf("materialize event launch %s: %w", launch.Admission.LaunchID, err)
		}
		if err = d.Triggers.Store.Acknowledge(ctx, launch.Admission.LaunchID); err != nil {
			return dispatched, fmt.Errorf("acknowledge event launch %s: %w", launch.Admission.LaunchID, err)
		}
		dispatched++
	}
	return dispatched, nil
}
