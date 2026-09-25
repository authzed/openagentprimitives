package approval

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestBlocksUntilDecision(t *testing.T) {
	o := New()
	want := Decision{Approved: true, ApproverID: "alice"}

	var (
		got     Decision
		gotErr  error
		done    = make(chan struct{})
		started = make(chan struct{})
	)
	go func() {
		close(started)
		d, err := o.Await(context.Background(), Request{
			RequestID:  "req-1",
			SessionRef: "default/sess-1",
		})
		got, gotErr = d, err
		close(done)
	}()

	<-started
	// Give the goroutine a chance to register and block.
	select {
	case <-done:
		t.Fatal("Await returned before DeliverDecision; expected to block")
	case <-time.After(50 * time.Millisecond):
	}

	// While Await is parked, the orchestrator should report 1 pending.
	assert.Equal(t, 1, o.PendingForSession("default/sess-1"), "PendingForSession during await")

	o.DeliverDecision("req-1", want)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Await did not return after DeliverDecision")
	}
	require.NoError(t, gotErr)
	assert.Equal(t, want, got)
	assert.Equal(t, 0, o.PendingForSession("default/sess-1"), "PendingForSession after delivery")
}

func TestRequestReturnsTimeoutOnCtxCancel(t *testing.T) {
	o := New()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	d, err := o.Await(ctx, Request{
		RequestID:  "req-tmo",
		SessionRef: "default/sess-1",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, "timeout", d.Reason)
	assert.False(t, d.Approved, "approved should be false on timeout")
	// Cleanup should have happened.
	assert.Equal(t, 0, o.PendingForSession("default/sess-1"), "PendingForSession after timeout")
}

func TestPendingCountForSession(t *testing.T) {
	o := New()
	const sess = "default/sess-X"

	var wg sync.WaitGroup
	wg.Add(2)
	started := make(chan struct{}, 2)
	for _, id := range []string{"req-a", "req-b"} {
		go func(id string) {
			defer wg.Done()
			started <- struct{}{}
			_, _ = o.Await(context.Background(), Request{
				RequestID:  id,
				SessionRef: sess,
			})
		}(id)
	}
	<-started
	<-started

	// Wait until both have registered as pending (or fail fast).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if o.PendingForSession(sess) == 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	require.Equal(t, 2, o.PendingForSession(sess), "PendingForSession with two awaits")

	o.DeliverDecision("req-a", Decision{Approved: true})
	// Wait until reported count drops.
	for time.Now().Before(deadline) {
		if o.PendingForSession(sess) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	assert.Equal(t, 1, o.PendingForSession(sess), "PendingForSession after one decision")

	o.DeliverDecision("req-b", Decision{Approved: false})
	wg.Wait()
	assert.Equal(t, 0, o.PendingForSession(sess), "PendingForSession after both decisions")
}

func TestDeliverDecisionForUnknownIsNoop(t *testing.T) {
	o := New()
	// Must not panic / block.
	require.NotPanics(t, func() {
		o.DeliverDecision("never-registered", Decision{Approved: true})
	})
}

func TestOnPublishIsInvoked(t *testing.T) {
	o := New()
	var called bool
	done := make(chan struct{})
	go func() {
		_, _ = o.Await(context.Background(), Request{
			RequestID:  "req-pub",
			SessionRef: "default/sess",
			OnPublish: func(_ context.Context) error {
				called = true
				return nil
			},
		})
		close(done)
	}()
	// Give Await a moment to invoke OnPublish then block.
	time.Sleep(20 * time.Millisecond)
	o.DeliverDecision("req-pub", Decision{Approved: true})
	<-done
	assert.True(t, called, "OnPublish was not invoked")
}

func TestEmptyRequestIDErrors(t *testing.T) {
	o := New()
	_, err := o.Await(context.Background(), Request{RequestID: ""})
	require.Error(t, err)
}
