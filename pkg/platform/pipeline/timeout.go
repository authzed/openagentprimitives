package pipeline

import (
	"context"
	"errors"
)

// IsTimeout reports whether err represents an approval-await deadline or
// cancellation. It is the shared classifier every Host uses to set the
// timedOut return of AwaitDecision, so no Host hand-rolls the errors.Is check
// and none can disagree about what "timed out" means.
func IsTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
