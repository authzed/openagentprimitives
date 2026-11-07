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
// backlog is logged as it grows — and, once the lane is full, how often its
// drops are. A lane only backs up when its own deliveries are slow, so this is
// the signal that names the session doing it.
const laneBacklogWarnEvery = 500

// DefaultMaxLaneBacklog caps one session's queued envelopes when
// Relay.MaxLaneBacklog is unset. A lane whose deliveries are wedged keeps
// receiving whatever its runner publishes; without a cap that is unbounded
// process memory. Past the cap the newest envelope is dropped and logged —
// the same newest-dropped behaviour the NATS client's slow-consumer limit gave
// the single subscription before lanes existed, but now confined to the one
// session that is stuck.
const DefaultMaxLaneBacklog = 10000

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
	handle     func(*nats.Msg)
	slots      chan struct{} // counting semaphore over concurrently-running handlers
	maxBacklog int
	ctx        context.Context

	mu    sync.Mutex
	lanes map[string]*lane
	idle  *sync.Cond // signalled whenever a lane is retired; used by wait
}

type lane struct {
	pending []*nats.Msg
	dropped int
}

func newSessionLanes(ctx context.Context, maxConcurrent, maxBacklog int, handle func(*nats.Msg)) *sessionLanes {
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrentSessions
	}
	if maxBacklog <= 0 {
		maxBacklog = DefaultMaxLaneBacklog
	}
	l := &sessionLanes{
		handle:     handle,
		slots:      make(chan struct{}, maxConcurrent),
		maxBacklog: maxBacklog,
		ctx:        ctx,
		lanes:      map[string]*lane{},
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
	if len(ln.pending) >= l.maxBacklog {
		ln.dropped++
		if ln.dropped == 1 || ln.dropped%laneBacklogWarnEvery == 0 {
			log.FromContext(l.ctx).Info("outbound relay: session backlog full; dropping its newest envelopes until it drains",
				"lane", key, "subject", msg.Subject, "queued", len(ln.pending), "dropped", ln.dropped)
		}
		l.mu.Unlock()
		return
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

// wait blocks until every lane has drained.
func (l *sessionLanes) wait() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.lanes) > 0 {
		l.idle.Wait()
	}
}

// waitCtx is wait bounded by ctx, for shutdown: it returns ctx's error if the
// lanes are still busy when ctx ends. The waiter it leaves behind in that case
// exits when the lanes finally drain.
func (l *sessionLanes) waitCtx(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		l.wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
