package slack

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitClosed fails the test if done is not closed within `within` — i.e. the
// ticker goroutine did not return (a leak / runaway loop).
func waitClosed(t *testing.T, done <-chan struct{}, within time.Duration, msg string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("%s: ticker goroutine did not return within %s (leak / runaway loop)", msg, within)
	}
}

// waitForUpdate polls the fake's chat.update calls (under its lock — the ticker
// writes them from another goroutine) until at least one lands, or fails.
func waitForUpdate(t *testing.T, fc *fakeSlackClient, within time.Duration) []updateCall {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if calls := fc.snapshotUpdateCalls(); len(calls) > 0 {
			return calls
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no chat.update recorded within %s", within)
	return nil
}

// updateText extracts the plain-text notification arg of a recorded chat.update
// (the same UnsafeApplyMsgOptions idiom the rest of this package's tests use).
func updateText(t *testing.T, c updateCall) string {
	t.Helper()
	_, vals, err := slackapi.UnsafeApplyMsgOptions("test-token", c.channelID, "http://test.invalid/", c.opts...)
	require.NoError(t, err, "UnsafeApplyMsgOptions")
	return vals.Get("text")
}

// TestRunInteractionTicker exercises the generic public-post expiry ticker's
// three termination paths — ctx-cancel, max-duration, update-error — proving it
// emits the formatElapsedSuffix caption and never leaks a goroutine or runs the
// update loop away. Reproduces the legacy runPendingTicker cadence for the
// generic interaction model (see interaction_ticker.go).
func TestRunInteractionTicker(t *testing.T) {
	t.Run("emits chat.update with the publisher's body + elapsed caption, then stops on ctx-cancel", func(t *testing.T) {
		fc := &fakeSlackClient{}
		s := &interactionSender{client: fc}
		ctx, cancel := context.WithCancel(context.Background())

		const body = "*demo-agent* wants to do a thing"
		done := make(chan struct{})
		go func() {
			s.runInteractionTicker(ctx, deliveryRef{ChannelID: "C1", TS: "100.1"},
				time.Now(), 5*time.Millisecond, body, logr.Discard())
			close(done)
		}()

		calls := waitForUpdate(t, fc, 2*time.Second)
		assert.Equal(t, "C1", calls[0].channelID, "edits the public-note channel")
		assert.Equal(t, "100.1", calls[0].ts, "edits the public-note ts")
		gotText := updateText(t, calls[0])
		assert.Contains(t, gotText, body,
			"caption must preserve the publisher's custom public-note body, not a content-free generic caption")
		// elapsed is well under a minute in a unit test ⇒ formatElapsedSuffix = "_just now._".
		assert.Contains(t, gotText, formatElapsedSuffix(30*time.Second),
			"caption must carry the formatElapsedSuffix suffix")

		// Cancel and confirm the goroutine returns (no leak) and stops updating.
		cancel()
		waitClosed(t, done, 2*time.Second, "ctx-cancel")
		n := len(fc.snapshotUpdateCalls())
		time.Sleep(25 * time.Millisecond)
		assert.Equal(t, n, len(fc.snapshotUpdateCalls()), "no chat.update after the ticker stopped")
	})

	t.Run("empty body still reads on the fixed lead alone", func(t *testing.T) {
		fc := &fakeSlackClient{}
		s := &interactionSender{client: fc}
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			s.runInteractionTicker(ctx, deliveryRef{ChannelID: "C1", TS: "100.1"},
				time.Now(), 5*time.Millisecond, "", logr.Discard())
			close(done)
		}()

		calls := waitForUpdate(t, fc, 2*time.Second)
		gotText := updateText(t, calls[0])
		assert.Contains(t, gotText, publicNoteLead,
			"the fixed lead carries the note when the publisher supplied no body")
		assert.NotContains(t, gotText, "⏳",
			"the tone chip is the affordance; the lead carries no glyph of its own")
		assert.Contains(t, gotText, formatElapsedSuffix(30*time.Second),
			"caption must still carry the formatElapsedSuffix suffix")

		cancel()
		waitClosed(t, done, 2*time.Second, "empty-body fallback")
	})

	t.Run("stops immediately on max-duration exceeded, without any update", func(t *testing.T) {
		fc := &fakeSlackClient{}
		s := &interactionSender{client: fc}

		done := make(chan struct{})
		go func() {
			// startedAt far enough in the past that the first tick's elapsed
			// exceeds awaitingMaxDuration → the loop returns before any chat.update.
			s.runInteractionTicker(context.Background(), deliveryRef{ChannelID: "C1", TS: "100.1"},
				time.Now().Add(-awaitingMaxDuration-time.Minute), 5*time.Millisecond, "some body", logr.Discard())
			close(done)
		}()

		waitClosed(t, done, 2*time.Second, "max-duration")
		assert.Empty(t, fc.snapshotUpdateCalls(), "max-duration must stop the ticker before any chat.update")
	})

	t.Run("stops on chat.update error after exactly one attempt", func(t *testing.T) {
		fc := &fakeSlackClient{updateErr: errors.New("slack: message_not_found")}
		s := &interactionSender{client: fc}

		done := make(chan struct{})
		go func() {
			s.runInteractionTicker(context.Background(), deliveryRef{ChannelID: "C1", TS: "100.1"},
				time.Now(), 5*time.Millisecond, "some body", logr.Discard())
			close(done)
		}()

		waitClosed(t, done, 2*time.Second, "update-error")
		assert.Len(t, fc.snapshotUpdateCalls(), 1, "a failed chat.update stops the ticker after one attempt")
	})

	t.Run("interval <= 0 returns without starting a ticker", func(t *testing.T) {
		fc := &fakeSlackClient{}
		s := &interactionSender{client: fc}
		done := make(chan struct{})
		go func() {
			s.runInteractionTicker(context.Background(), deliveryRef{ChannelID: "C1", TS: "100.1"},
				time.Now(), 0, "some body", logr.Discard())
			close(done)
		}()
		waitClosed(t, done, 2*time.Second, "non-positive interval")
		assert.Empty(t, fc.snapshotUpdateCalls(), "a non-positive interval must not emit any update")
	})
}

// TestRunInteractionTicker_LaunchCancel covers the launch/cancel map wiring:
// launchExpiryTicker tracks a cancel handle keyed by RequestRef, cancelExpiryTicker
// stops it and drops the entry, and double-cancel / missing-key / guarded-no-op
// launches are all safe. The map is mutex-guarded; the -race suite enforces no
// data race between the request and applied paths.
func TestRunInteractionTicker_LaunchCancel(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc}

	// Cancel before any launch (unknown key) is a safe no-op.
	assert.NotPanics(t, func() { s.cancelExpiryTicker("never-launched") })

	s.launchExpiryTicker(deliveryRef{ChannelID: "C1", TS: "100.1"}, "req-1", time.Now(), "some body", logr.Discard())
	s.tickMu.Lock()
	_, tracked := s.tickCancel["req-1"]
	s.tickMu.Unlock()
	require.True(t, tracked, "launch must track the cancel handle under RequestRef")

	s.cancelExpiryTicker("req-1")
	s.tickMu.Lock()
	_, stillTracked := s.tickCancel["req-1"]
	s.tickMu.Unlock()
	assert.False(t, stillTracked, "cancel must drop the map entry")

	// Double-cancel: safe no-op.
	assert.NotPanics(t, func() { s.cancelExpiryTicker("req-1") })

	// Guarded no-op launches: empty TS or empty RequestRef must not track anything
	// (and must not leak a goroutine).
	s.launchExpiryTicker(deliveryRef{ChannelID: "C1", TS: ""}, "req-2", time.Now(), "some body", logr.Discard())
	s.launchExpiryTicker(deliveryRef{ChannelID: "C1", TS: "100.3"}, "", time.Now(), "some body", logr.Discard())
	s.tickMu.Lock()
	_, r2 := s.tickCancel["req-2"]
	n := len(s.tickCancel)
	s.tickMu.Unlock()
	assert.False(t, r2, "empty-TS launch must be a guarded no-op")
	assert.Equal(t, 0, n, "no guarded no-op launch should leave a map entry")
}

// TestRunInteractionTicker_DoubleLaunchSupersedes covers Fix 2's mutex-ordering
// change in launchExpiryTicker: the "supersede an existing ticker" branch now
// grabs prev under s.tickMu and calls prev.cancel() AFTER Unlock() (never holds
// tickMu across cancel(), matching cancelExpiryTicker's own policy comment).
// Launching twice for the SAME RequestRef while the first ticker's goroutine is
// still alive must leave exactly one map entry — the second (newer) handle —
// and the first goroutine's self-cleanup (which races the second launch) must
// not clobber it. If launchExpiryTicker instead held tickMu across cancel(),
// the first goroutine's self-cleanup (which also needs tickMu) could deadlock
// against it; this test's timeout plus -race is what would catch that.
func TestRunInteractionTicker_DoubleLaunchSupersedes(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc}

	s.launchExpiryTicker(deliveryRef{ChannelID: "C1", TS: "200.1"}, "req-double", time.Now(), "first body", logr.Discard())
	s.tickMu.Lock()
	h1 := s.tickCancel["req-double"]
	s.tickMu.Unlock()
	require.NotNil(t, h1, "first launch must track a handle")

	// Second launch for the same RequestRef while the first ticker's goroutine
	// is still alive (blocked on its own 30s production interval, since
	// launchExpiryTicker always drives runInteractionTicker at awaitingTickInterval) —
	// this must supersede it.
	s.launchExpiryTicker(deliveryRef{ChannelID: "C1", TS: "200.2"}, "req-double", time.Now(), "second body", logr.Discard())
	s.tickMu.Lock()
	h2 := s.tickCancel["req-double"]
	s.tickMu.Unlock()
	require.NotNil(t, h2, "second launch must track a handle")
	assert.NotSame(t, h1, h2, "second launch must install a distinct handle, not reuse the first")

	// The superseded first ticker observes ctx-cancel and self-cleans in its own
	// goroutine; launchExpiryTicker gives no direct completion signal, so poll
	// (bounded) until the map settles rather than sleeping a fixed guess.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.tickMu.Lock()
		n := len(s.tickCancel)
		s.tickMu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	s.tickMu.Lock()
	got, ok := s.tickCancel["req-double"]
	n := len(s.tickCancel)
	s.tickMu.Unlock()
	assert.Equal(t, 1, n, "only the newer handle survives; the superseded goroutine's self-cleanup must not leave a stale or extra entry")
	require.True(t, ok, "the surviving entry must still be keyed by req-double")
	assert.Same(t, h2, got, "the superseded ticker's self-cleanup must not clobber the newer handle installed by the second launch")

	// Clean up the still-running second ticker so it doesn't leak past this test.
	s.cancelExpiryTicker("req-double")
}
