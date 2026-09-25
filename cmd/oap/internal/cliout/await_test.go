package cliout

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A bytes.Buffer is not an *os.File, so colorize() is false and Await takes its
// non-TTY path (no animation, plain status lines) — keeping these tests fast and
// deterministic.

func TestAwait_DoneOnFirstPoll_NoBackoffWait(t *testing.T) {
	var buf bytes.Buffer
	calls := 0
	err := Await(context.Background(), &buf, "waiting", time.Second, func(context.Context) (bool, error) {
		calls++
		return true, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "done on first poll must not wait/backoff")
}

func TestAwait_Timeout_ReturnsDeadlineExceeded(t *testing.T) {
	var buf bytes.Buffer
	err := Await(context.Background(), &buf, "waiting", 50*time.Millisecond, func(context.Context) (bool, error) {
		return false, nil // never done → deadline wins before the 2s backoff
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestAwait_PollError_PropagatesImmediately(t *testing.T) {
	var buf bytes.Buffer
	boom := errors.New("boom")
	calls := 0
	err := Await(context.Background(), &buf, "waiting", time.Second, func(context.Context) (bool, error) {
		calls++
		return false, boom
	})
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 1, calls, "a poll error aborts without retrying")
}
