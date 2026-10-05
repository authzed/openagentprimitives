package sessionevents

import (
	"context"
	"errors"
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
	After    string
}

// LaunchPager lets a polling consumer advance past failing work. The cursor
// wraps after a complete scan, so newly inserted earlier IDs remain eligible.
type LaunchPager interface {
	PendingAfter(context.Context, time.Time, string, int) ([]Launch, error)
}

// Drain leaves a launch pending on failure. If materialization committed but its
// acknowledgment was lost, the next drain repeats the same idempotent request.
// Retries preserve the original launch identity and recheck current authority.
func (d *Dispatcher) Drain(ctx context.Context, now time.Time, limit int) (int, error) {
	if d == nil || d.Triggers == nil || d.Triggers.Store == nil || d.Consumer == nil {
		return 0, ErrDenied
	}
	var launches []Launch
	var err error
	if pager, ok := d.Triggers.Store.(LaunchPager); ok {
		launches, err = pager.PendingAfter(ctx, now, d.After, limit)
	} else {
		launches, err = d.Triggers.Store.Pending(ctx, now, limit)
	}
	if err != nil {
		return 0, err
	}
	if len(launches) == 0 {
		d.After = ""
	}
	dispatched := 0
	var failures []error
	for _, launch := range launches {
		if err = ctx.Err(); err != nil {
			return dispatched, err
		}
		d.After = launch.Admission.LaunchID
		if err = d.Triggers.CheckLaunch(ctx, launch, now); err != nil {
			failures = append(failures, fmt.Errorf("authorize event launch %s: %w", launch.Admission.LaunchID, err))
			if errors.Is(err, ErrDenied) {
				if stopErr := d.Triggers.Store.StopSubscription(ctx, launch.Subscription.ID, "launch_authority_denied"); stopErr != nil {
					failures = append(failures, stopErr)
				}
			}
			continue
		}
		if err = d.Consumer.Materialize(ctx, launch); err != nil {
			failures = append(failures, fmt.Errorf("materialize event launch %s: %w", launch.Admission.LaunchID, err))
			continue
		}
		if err = d.Triggers.Store.Acknowledge(ctx, launch.Admission.LaunchID); err != nil {
			failures = append(failures, fmt.Errorf("acknowledge event launch %s: %w", launch.Admission.LaunchID, err))
			continue
		}
		dispatched++
	}
	return dispatched, errors.Join(failures...)
}
