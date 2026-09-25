package testenv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTestWriter_FollowsTheRunningTest pins the fix for a diagnosability defect
// that cost a full debugging session.
//
// controller-runtime's SetLogger is effectively write-once — DelegatingLogSink
// accepts the first logger and ignores every later call — so a writer that
// captured a *testing.T bound EVERY controller and channelsd log line in the
// process to whichever test ran first. In a package with more than one test,
// every test but that one logged into the void, which makes "did this code path
// execute at all?" unanswerable exactly where you most need to ask it.
//
// The writer must therefore follow the running test rather than the first one.
func TestTestWriter_FollowsTheRunningTest(t *testing.T) {
	// Stand in for two sequential Start(t) calls without booting envtest.
	first := &testing.T{}
	currentT.Store(first)

	var second *testing.T
	t.Run("a later test claims the writer", func(sub *testing.T) {
		second = sub
		currentT.Store(sub)
		require.Same(t, sub, currentT.Load(),
			"the most recent Start owns the writer, not the first")

		n, err := testWriter{}.Write([]byte("line for the running test"))
		require.NoError(t, err, "the writer never fails the caller")
		assert.Equal(t, len("line for the running test"), n, "byte count")
	})

	// The subtest's cleanup analogue: a finished test must release the writer
	// so nothing writes into a completed t (which panics), but only if it is
	// still the owner.
	currentT.CompareAndSwap(second, nil)
	assert.Nil(t, currentT.Load(), "a finished test releases the writer")

	// …and with no owner, a stray line from a goroutine that outlived its test
	// is dropped rather than panicking.
	assert.NotPanics(t, func() {
		_, _ = testWriter{}.Write([]byte("stray line after the test finished"))
	}, "an ownerless write must be dropped, never panic")
}

// TestTestWriter_CompareAndSwapDoesNotStealFromASibling covers why the cleanup
// is a CAS rather than a plain clear: when a sibling test has already claimed
// the writer, the finishing test must not silence it.
func TestTestWriter_CompareAndSwapDoesNotStealFromASibling(t *testing.T) {
	finishing := &testing.T{}
	sibling := &testing.T{}

	currentT.Store(finishing)
	currentT.Store(sibling) // the sibling's Start ran before finishing's cleanup

	currentT.CompareAndSwap(finishing, nil)
	assert.Same(t, sibling, currentT.Load(),
		"a finishing test must not clear a writer another test now owns")

	currentT.Store(nil) // leave package state clean for other tests
}
