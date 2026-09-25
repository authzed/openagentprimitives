package installcmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtendableTimeout_FiresWithDeadlineExceeded confirms an un-reset window
// times out and reports context.DeadlineExceeded (so the readiness loop still
// treats it as "offer keep-waiting", not a hard Ctrl-C stop).
func TestExtendableTimeout_FiresWithDeadlineExceeded(t *testing.T) {
	ctx, _, cancel := newExtendableTimeout(context.Background(), 20*time.Millisecond)
	t.Cleanup(cancel)

	select {
	case <-ctx.Done():
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("extendable timeout never fired")
	}
}

// TestExtendableTimeout_ResetExtendsWindow is the regression for the post-AI-fix
// "context deadline exceeded": resetting before the window elapses must restart
// the clock, so the context is still live well past the original deadline.
func TestExtendableTimeout_ResetExtendsWindow(t *testing.T) {
	ctx, reset, cancel := newExtendableTimeout(context.Background(), 40*time.Millisecond)
	t.Cleanup(cancel)

	// Simulate a long interactive fix session, resetting on the way out.
	time.Sleep(25 * time.Millisecond)
	reset()

	// Past the ORIGINAL 40ms deadline it must still be live (reset pushed it out).
	time.Sleep(30 * time.Millisecond) // 55ms total > 40ms original
	assert.NoError(t, ctx.Err(), "reset must extend the window past the original deadline")

	// And it still eventually fires on the fresh window.
	select {
	case <-ctx.Done():
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("extendable timeout never fired after reset")
	}
}

// TestExtendableTimeout_PropagatesParentCancel proves SIGINT (modeled as parent
// cancel) still tears the context down promptly.
func TestExtendableTimeout_PropagatesParentCancel(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	ctx, _, cancel := newExtendableTimeout(parent, time.Hour)
	t.Cleanup(cancel)

	parentCancel()
	select {
	case <-ctx.Done():
		assert.ErrorIs(t, ctx.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not propagate")
	}
}

// TestExtendableTimeout_ResetAfterDoneIsNoop ensures a reset arriving after the
// window already fired does not resurrect the context.
func TestExtendableTimeout_ResetAfterDoneIsNoop(t *testing.T) {
	ctx, reset, cancel := newExtendableTimeout(context.Background(), 10*time.Millisecond)
	t.Cleanup(cancel)

	<-ctx.Done()
	reset() // must not un-cancel
	assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
}

// TestExtendableTimeout_ValueDelegates confirms context values (external-tool
// timeout, the reset handle) still resolve through the extendable context, and
// that withDeadlineReset/resetInstallDeadline round-trip.
func TestExtendableTimeout_ValueDelegates(t *testing.T) {
	type k struct{}
	parent := context.WithValue(context.Background(), k{}, "v")
	ctx, reset, cancel := newExtendableTimeout(parent, time.Hour)
	t.Cleanup(cancel)
	require.Equal(t, "v", ctx.Value(k{}), "parent values must delegate through")

	// resetInstallDeadline finds the reset handle even after further wrapping.
	var called bool
	wrapped := withDeadlineReset(ctx, func() { called = true; reset() })
	wrapped = context.WithValue(wrapped, k{}, "shadow") // simulate later wrapping
	resetInstallDeadline(wrapped)
	assert.True(t, called, "resetInstallDeadline must invoke the carried reset func")
}
