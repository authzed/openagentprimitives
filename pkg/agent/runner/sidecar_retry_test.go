package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// TestRetryingProber_RetriesUntilSuccess pins the in-pod sidecar boot-probe race
// fix. In-pod sidecars are regular pod containers that start in PARALLEL with the
// runner (no ordering guarantee), so the runner can dial the sidecar's MCP
// endpoint a beat before its server binds — a transient "connection refused"
// (verified live: the sidecar logged "listening on :18080" AFTER the one-shot
// boot probe had already failed the session). The boot prober must retry within
// the deadline, not fail the whole session on the first refusal.
func TestRetryingProber_RetriesUntilSuccess(t *testing.T) {
	calls := 0
	base := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("dial tcp 127.0.0.1:18080: connect: connection refused")
		}
		return []mcpprobe.Tool{{Name: "noop"}}, nil
	}
	got, err := RetryingProber(base, time.Second, 5*time.Millisecond)(context.Background(), "http://x")
	require.NoError(t, err, "the boot prober must retry a not-yet-bound sidecar, not fail on the first connection-refused")
	assert.GreaterOrEqual(t, calls, 3, "must retry until the endpoint binds")
	require.Len(t, got, 1)
	assert.Equal(t, "noop", got[0].Name)
}

// TestRetryingProber_DeadlineReturnsLastErr — a sidecar that never binds must
// still fail (after the deadline), and surface the real underlying error so the
// SidecarBootFailed message is diagnosable.
func TestRetryingProber_DeadlineReturnsLastErr(t *testing.T) {
	calls := 0
	base := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		calls++
		return nil, errors.New("connection refused")
	}
	_, err := RetryingProber(base, 30*time.Millisecond, 5*time.Millisecond)(context.Background(), "http://x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused", "the terminal error must carry the real probe failure")
	assert.Greater(t, calls, 1, "must have retried before giving up at the deadline")
}

// TestRetryingProber_CancelledCtxReturnsPromptly — a cancelled reconcile/boot ctx
// abandons the retry loop at once (naming cancellation), never waiting out the
// deadline.
func TestRetryingProber_CancelledCtxReturnsPromptly(t *testing.T) {
	base := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		return nil, errors.New("refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := RetryingProber(base, time.Hour, 10*time.Millisecond)(ctx, "http://x")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second, "must abandon the retry on cancellation, not wait the deadline")
}
