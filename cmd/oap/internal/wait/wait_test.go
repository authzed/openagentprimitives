package wait_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
)

func TestUntilSucceedsImmediately(t *testing.T) {
	ctx := context.Background()
	called := 0
	err := wait.Until(ctx, 50*time.Millisecond, time.Second, func(context.Context) (bool, error) {
		called++
		return true, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, called)
}

func TestUntilEventuallySucceeds(t *testing.T) {
	ctx := context.Background()
	called := 0
	err := wait.Until(ctx, 5*time.Millisecond, time.Second, func(context.Context) (bool, error) {
		called++
		return called >= 3, nil
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, called, 3)
}

func TestUntilTimeoutReturnsErrTimeout(t *testing.T) {
	ctx := context.Background()
	err := wait.Until(ctx, 5*time.Millisecond, 30*time.Millisecond, func(context.Context) (bool, error) {
		return false, nil
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, wait.ErrTimeout)
}

func TestUntilPropagatesParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := wait.Until(ctx, 5*time.Millisecond, time.Hour, func(context.Context) (bool, error) {
		return false, nil
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestUntilPropagatesProbeError(t *testing.T) {
	ctx := context.Background()
	probeErr := errors.New("boom")
	err := wait.Until(ctx, 5*time.Millisecond, time.Second, func(context.Context) (bool, error) {
		return false, probeErr
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, probeErr)
}
