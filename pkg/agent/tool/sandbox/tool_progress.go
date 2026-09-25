package sandbox

import (
	"context"
	"time"
)

const (
	// toolProgressThreshold is how long a sync tool must run before its first
	// liveness tick — "more than a few seconds", so fast tools never flicker.
	toolProgressThreshold = 3 * time.Second
	// toolProgressInterval is the re-emit cadence once a tool is being shown.
	toolProgressInterval = 3 * time.Second
)

// ToolProgressUpdate is one liveness/progress snapshot for a running sync tool.
// CallID/Name are stamped by the runner's injected hook closure; the sandbox
// tool fills the timing/budget (and, in Layer 2, the progress) fields.
type ToolProgressUpdate struct {
	CallID         string
	Name           string
	BudgetSeconds  int
	ElapsedSeconds int
	Done           bool
	// Layer 2 (nil/empty in Layer 1):
	Percent    *int32
	TailLine   string
	EtaSeconds *int32
}

// ToolProgressHook lets the runner publish tool-progress snapshots while a sync
// tool executes. Injected via WithToolProgressHook; a zero hook (nil Emit) is a
// no-op so kubectl-driven sessions and tests need no wiring.
type ToolProgressHook struct {
	Emit func(ctx context.Context, u ToolProgressUpdate)
}

type toolProgressHookKey struct{}

// WithToolProgressHook returns a context carrying h for the sync poll loop.
func WithToolProgressHook(ctx context.Context, h ToolProgressHook) context.Context {
	return context.WithValue(ctx, toolProgressHookKey{}, h)
}

// ToolProgressHookFromCtx returns the hook set by WithToolProgressHook.
func ToolProgressHookFromCtx(ctx context.Context) (ToolProgressHook, bool) {
	h, ok := ctx.Value(toolProgressHookKey{}).(ToolProgressHook)
	return h, ok
}

// toolProgressTick decides whether to emit a snapshot on this poll tick. Emits
// once elapsed crosses the threshold, then no more often than the interval.
// Pure — unit-testable without a live pod.
func toolProgressTick(now, start, lastEmit time.Time, threshold, interval time.Duration) (emit bool, elapsed time.Duration) {
	elapsed = now.Sub(start)
	if elapsed < threshold {
		return false, elapsed
	}
	if !lastEmit.IsZero() && now.Sub(lastEmit) < interval {
		return false, elapsed
	}
	return true, elapsed
}
