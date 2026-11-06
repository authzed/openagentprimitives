package outbound

import (
	"context"
	"sync"

	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultMaxConcurrentSessions bounds how many sessions' envelopes the relay
// works on at once when Relay.MaxConcurrentSessions is unset. Bounded rather
// than one-per-session because every lane ends in a channel kind's API (Slack's
// is rate-limited per workspace), and a burst of sessions should queue here, in
// order, rather than as a burst of 429s.
const DefaultMaxConcurrentSessions = 16

// laneBacklogWarnEvery is how often (in queued envelopes) a single session's
// backlog is logged as it grows. A lane only backs up when its own deliveries
// are slow, so this is the signal that names the session doing it.
const laneBacklogWarnEvery = 500

// sessionLanes runs the relay's handler concurrently ACROSS sessions and
// strictly in arrival order WITHIN one.
//
// nats.go dispatches a subscription's callbacks on a single goroutine, and the
// relay subscribes to every session's outbound tree with one subscription, so
// handling inline put every session in one line: a slow delivery for one
// session — a rate-limited API call, a slow upload — held up every other
// session's replies and approval prompts behind it. The NATS callback now only
// enqueues; each session with work queued has one goroutine draining its own
// lane, so its envelopes still reach the channel in the order the runner
// published them (stream deltas before the reply that finalises them, a card
// before its resolution).
//
// A lane's goroutine exits as soon as its queue is empty, so idle sessions cost
// nothing. The queue is unbounded on purpose: blocking the NATS callback on one
// full lane would put every session back in one line, and dropping would lose
// user-visible output. A growing lane is logged instead.
type sessionLanes struct {
	handle func(*nats.Msg)
	slots  chan struct{} // counting semaphore over concurrently-running handlers
	ctx    context.Context

	mu    sync.Mutex
	lanes map[string]*lane
	idle  *sync.Cond // signalled whenever a lane is retired; used by wait
}

type lane struct {
	pending []*nats.Msg
}

func newSessionLanes(ctx context.Context, maxConcurrent int, handle func(*nats.Msg)) *sessionLanes {
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrentSessions
	}
	l := &sessionLanes{
		handle: handle,
		slots:  make(chan struct{}, maxConcurrent),
		ctx:    ctx,
		lanes:  map[string]*lane{},
	}
	l.idle = sync.NewCond(&l.mu)
	return l
}

// dispatch enqueues msg on its session's lane, starting the lane's drainer if
// it is not already running. Never blocks on handler work.
func (l *sessionLanes) dispatch(key string, msg *nats.Msg) {
	l.mu.Lock()
	ln, running := l.lanes[key]
	if !running {
		ln = &lane{}
		l.lanes[key] = ln
	}
	ln.pending = append(ln.pending, msg)
	if n := len(ln.pending); n%laneBacklogWarnEvery == 0 {
		log.FromContext(l.ctx).Info("outbound relay: session backlog growing; its deliveries are slower than its output",
			"lane", key, "queued", n)
	}
	l.mu.Unlock()
	if !running {
		go l.drain(key, ln)
	}
}

func (l *sessionLanes) drain(key string, ln *lane) {
	for {
		l.mu.Lock()
		if len(ln.pending) == 0 {
			delete(l.lanes, key)
			l.idle.Broadcast()
			l.mu.Unlock()
			return
		}
		msg := ln.pending[0]
		ln.pending[0] = nil
		ln.pending = ln.pending[1:]
		l.mu.Unlock()

		// A slot per ENVELOPE, not per lane: a session with a long backlog
		// yields between envelopes, so it cannot hold a slot while others wait.
		l.slots <- struct{}{}
		l.handle(msg)
		<-l.slots
	}
}

// wait blocks until every lane has drained. For tests and orderly shutdown.
func (l *sessionLanes) wait() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.lanes) > 0 {
		l.idle.Wait()
	}
}
