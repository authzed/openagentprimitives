// Package wait provides a generic "poll until condition" helper used by
// oap init and oap agent run.
package wait

import (
	"context"
	"errors"
	"time"
)

// ErrTimeout is returned by Until when the deadline elapses before fn
// returns (true, nil).
var ErrTimeout = errors.New("wait: timeout")

// Until polls fn at every interval until fn returns (true, nil), the deadline
// elapses (returns ErrTimeout), or fn returns a non-nil error (returned wrapped).
func Until(ctx context.Context, interval, deadline time.Duration, fn func(context.Context) (bool, error)) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	for {
		ok, err := fn(deadlineCtx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-deadlineCtx.Done():
			if ctx.Err() != nil {
				// Parent cancelled (e.g., Ctrl-C): propagate the parent's error.
				return ctx.Err()
			}
			return ErrTimeout
		case <-time.After(interval):
		}
	}
}
