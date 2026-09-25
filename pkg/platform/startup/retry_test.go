package startup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetry_SucceedsFirstTry_NoOnRetryCalls(t *testing.T) {
	calls, retries := 0, 0
	err := retryWithBackoff(context.Background(), "op", time.Second, time.Millisecond, 2*time.Millisecond,
		func(context.Context) error { calls++; return nil },
		func(int, error, time.Duration) { retries++ })
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, 0, retries)
}

func TestRetry_SucceedsAfterTransientFailures(t *testing.T) {
	calls, retries := 0, 0
	lastAttempt := 0
	err := retryWithBackoff(context.Background(), "op", time.Second, time.Millisecond, 2*time.Millisecond,
		func(context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("not ready")
			}
			return nil
		},
		func(attempt int, _ error, _ time.Duration) { retries++; lastAttempt = attempt })
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
	assert.Equal(t, 2, retries)
	assert.Equal(t, 2, lastAttempt) // onRetry fires on attempts 1 and 2
}

func TestRetry_CeilingExceeded_ReturnsWrappedTerminalError(t *testing.T) {
	sentinel := errors.New("connection refused")
	err := retryWithBackoff(context.Background(), "connect to postgres", 5*time.Millisecond, time.Millisecond, 2*time.Millisecond,
		func(context.Context) error { return sentinel },
		nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect to postgres")
	assert.ErrorIs(t, err, sentinel)
}

func TestRetry_ContextCancelled_ReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	start := time.Now()
	err := retryWithBackoff(ctx, "op", time.Minute, 50*time.Millisecond, 100*time.Millisecond,
		func(context.Context) error { return errors.New("not ready") },
		nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second)
}

func TestRetry_PublicWrapperDelegates(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), "op", time.Second,
		func(context.Context) error { calls++; return nil }, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}
