package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// One plain NATS subscription serves every session's metaagent requests, and
// nats.go dispatches a subscription's callbacks serially. Handle ran on that
// callback and BLOCKED when a session's 16-slot channel filled, with only
// ctx.Done() as an escape — and the per-session goroutine that drains it can
// sit inside a 24-hour approval await.
//
// So one session that produces a card nobody clicks, plus 16 more requests,
// stops metaagent delivery for the WHOLE CLUSTER: every scope-enabled session
// then halts fail-closed at cold start waiting for a task that will never
// arrive. The authors' own comment names the wedge and cures only the
// shutdown case.
//
// It does not need an attacker. A class in shadow mode posts real approval
// cards for ordinary owner messages, so a chatty owner reaches this by
// accident.
//
// The sibling Worker.Handle already drops with a log rather than blocking.
func TestMetaagentHandle_DoesNotBlockWhenASessionsQueueIsFull(t *testing.T) {
	w := &MetaagentWorker{sessions: map[string]chan metaagentRequest{}}
	// A session whose goroutine never drains: exactly the shape of one parked
	// on a human approval.
	key := "ns/stuck"
	w.sessions[key] = make(chan metaagentRequest, 1)

	in := HandleInput{Scope: memory.Scope{Kind: "session", ID: key}, Requester: "user:a", Text: "widen"}

	// Fill the buffer, then overflow it. The overflow must return rather than
	// park the caller — the caller here is the shared subscription callback.
	require.NoError(t, w.Handle(context.Background(), in))

	done := make(chan error, 1)
	go func() { done <- w.Handle(context.Background(), in) }()

	select {
	case err := <-done:
		assert.NoError(t, err, "a full queue is a drop, not a delivery failure the bus should retry")
	case <-time.After(2 * time.Second):
		t.Fatal("Handle blocked on a full session queue — one session can wedge metaagent delivery cluster-wide")
	}
}

// A session that IS draining must still receive its request: the drop is for a
// backlogged session, not a general degradation.
func TestMetaagentHandle_DeliversToADrainingSession(t *testing.T) {
	w := &MetaagentWorker{sessions: map[string]chan metaagentRequest{}}
	key := "ns/live"
	ch := make(chan metaagentRequest, 1)
	w.sessions[key] = ch

	in := HandleInput{Scope: memory.Scope{Kind: "session", ID: key}, Requester: "user:a", Text: "widen"}
	require.NoError(t, w.Handle(context.Background(), in))

	select {
	case got := <-ch:
		assert.Equal(t, "widen", got.text)
	case <-time.After(time.Second):
		t.Fatal("a request for a session with room must be delivered")
	}
}

// One session's backlog must not affect another's. This is the property the
// blocking send destroyed: the shared callback was the shared resource.
func TestMetaagentHandle_OneStuckSessionDoesNotStopAnother(t *testing.T) {
	w := &MetaagentWorker{sessions: map[string]chan metaagentRequest{}}
	stuck, live := "ns/stuck", "ns/live"
	w.sessions[stuck] = make(chan metaagentRequest, 1)
	liveCh := make(chan metaagentRequest, 1)
	w.sessions[live] = liveCh

	stuckIn := HandleInput{Scope: memory.Scope{Kind: "session", ID: stuck}, Requester: "user:a", Text: "x"}
	require.NoError(t, w.Handle(context.Background(), stuckIn))
	require.NoError(t, w.Handle(context.Background(), stuckIn)) // overflow, dropped

	liveIn := HandleInput{Scope: memory.Scope{Kind: "session", ID: live}, Requester: "user:b", Text: "y"}
	require.NoError(t, w.Handle(context.Background(), liveIn))

	select {
	case got := <-liveCh:
		assert.Equal(t, "y", got.text, "a healthy session must be unaffected by a backlogged one")
	case <-time.After(time.Second):
		t.Fatal("a backlogged session starved an unrelated one")
	}
}
