package outbound

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func laneMsg(key string, i int) *nats.Msg {
	return &nats.Msg{Subject: key, Data: []byte(fmt.Sprint(i))}
}

// The failure this exists to prevent: one session's slow delivery held every
// other session's output behind it, for twenty minutes on a live cluster.
func TestSessionLanes_BlockedSessionDoesNotHoldUpOthers(t *testing.T) {
	release := make(chan struct{})
	delivered := make(chan string, 1)
	lanes := newSessionLanes(context.Background(), 4, 0, func(m *nats.Msg) {
		if m.Subject == "slow" {
			<-release
			return
		}
		delivered <- m.Subject
	})

	lanes.dispatch("slow", laneMsg("slow", 0))
	lanes.dispatch("fast", laneMsg("fast", 0))

	select {
	case got := <-delivered:
		assert.Equal(t, "fast", got)
	case <-time.After(5 * time.Second):
		t.Fatal("a session's envelope waited on another session's stuck delivery")
	}
	close(release)
	lanes.wait()
}

func TestSessionLanes_PreservesOrderWithinASession(t *testing.T) {
	const sessions, perSession = 8, 200
	var mu sync.Mutex
	seen := map[string][]string{}
	lanes := newSessionLanes(context.Background(), 3, 0, func(m *nats.Msg) {
		mu.Lock()
		seen[m.Subject] = append(seen[m.Subject], string(m.Data))
		mu.Unlock()
	})

	for i := 0; i < perSession; i++ {
		for s := 0; s < sessions; s++ {
			lanes.dispatch(fmt.Sprintf("s%d", s), laneMsg(fmt.Sprintf("s%d", s), i))
		}
	}
	lanes.wait()

	require.Len(t, seen, sessions, "every session's envelopes were handled")
	for s := 0; s < sessions; s++ {
		key := fmt.Sprintf("s%d", s)
		want := make([]string, perSession)
		for i := range want {
			want[i] = fmt.Sprint(i)
		}
		assert.Equal(t, want, seen[key], "session %s handled in publish order", key)
	}
}

func TestSessionLanes_BoundsConcurrency(t *testing.T) {
	const limit = 3
	var running, peak atomic.Int32
	lanes := newSessionLanes(context.Background(), limit, 0, func(*nats.Msg) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
	})

	for i := 0; i < 40; i++ {
		lanes.dispatch(fmt.Sprintf("s%d", i), laneMsg("x", i))
	}
	lanes.wait()

	assert.LessOrEqual(t, peak.Load(), int32(limit), "never more handlers in flight than the limit")
	assert.Greater(t, peak.Load(), int32(1), "sessions did run concurrently")
}

func TestSessionLanes_IdleLaneIsRetired(t *testing.T) {
	lanes := newSessionLanes(context.Background(), 2, 0, func(*nats.Msg) {})
	lanes.dispatch("s", laneMsg("s", 0))
	lanes.wait()

	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	assert.Empty(t, lanes.lanes, "a drained lane is removed, so idle sessions hold no goroutine or entry")
}

// A wedged delivery must not hold its concurrency slot forever: channel-kind
// API clients are not guaranteed a timeout of their own, and enough wedged
// sessions at once would put every other session back in line.
func TestRelay_WedgedDeliveryReleasesItsSlotAtTheDeadline(t *testing.T) {
	nc := connectNATS(t)
	bar := defaultSession()
	bar.Name = "bar"
	bar.Spec.InputChannel.NATSSubjectPrefix = "ap.session.default.bar"
	cli := fakeClientWith(t, defaultSession(), bar)

	sndr := &wedgingSender{wedge: "foo", abandoned: make(chan struct{}, 1)}
	startRelay(t, nc, cli, &fixedResolver{s: sndr}, func(r *Relay) {
		r.MaxConcurrentSessions = 1 // one slot: foo's wedged send holds it
		r.HandleTimeout = 200 * time.Millisecond
	})

	msg := channelevents.OutboundUserMessagePayload{Text: "hi"}
	publishOut(t, nc, "default", "foo", string(channelevents.KindUserMessage), buildEnv(t, "foo", channelevents.KindUserMessage, msg))
	publishOut(t, nc, "default", "bar", string(channelevents.KindUserMessage), buildEnv(t, "bar", channelevents.KindUserMessage, msg))

	require.True(t, waitUntil(t, 5*time.Second, func() bool { return sndr.delivered("bar") }),
		"bar was never delivered: foo's wedged send kept the only slot")
	select {
	case <-sndr.abandoned:
	default:
		t.Fatal("foo's send was never cancelled, so its slot was not released by the deadline")
	}
}

// wedgingSender never returns for one session until its context ends, and
// records deliveries for every other.
type wedgingSender struct {
	wedge     string
	abandoned chan struct{}
	mu        sync.Mutex
	got       []string
}

func (s *wedgingSender) Send(ctx context.Context, sess channelkinds.SessionInfo, _ channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	if sess.Name == s.wedge {
		<-ctx.Done()
		// Non-blocking: the relay's failure fallback sends again on the same
		// (already expired) context, and only the first abandonment matters.
		select {
		case s.abandoned <- struct{}{}:
		default:
		}
		return channelkinds.SubChannelSendResult{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, sess.Name)
	return channelkinds.SubChannelSendResult{}, nil
}

func (s *wedgingSender) delivered(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.got, name)
}

func TestSessionLanes_FullLaneDropsNewestAndKeepsOthersFlowing(t *testing.T) {
	release, wedged := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var handled []string
	lanes := newSessionLanes(context.Background(), 4, 3, func(m *nats.Msg) {
		if m.Subject == "stuck" && string(m.Data) == "0" {
			close(wedged)
			<-release
		}
		mu.Lock()
		handled = append(handled, m.Subject+":"+string(m.Data))
		mu.Unlock()
	})

	// Message 0 is taken off the queue and wedges; then 1-3 fill the lane to
	// its cap of 3, and 4 and 5 are dropped.
	lanes.dispatch("stuck", laneMsg("stuck", 0))
	<-wedged
	for i := 1; i <= 5; i++ {
		lanes.dispatch("stuck", laneMsg("stuck", i))
	}
	lanes.dispatch("other", laneMsg("other", 0))
	require.True(t, waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(handled, "other:0")
	}), "another session must keep flowing past a full lane")

	lanes.mu.Lock()
	assert.Equal(t, 2, lanes.lanes["stuck"].dropped, "the two envelopes past the cap were dropped, not queued")
	lanes.mu.Unlock()

	close(release)
	lanes.wait()
	mu.Lock()
	defer mu.Unlock()
	var stuck []string
	for _, h := range handled {
		if strings.HasPrefix(h, "stuck:") {
			stuck = append(stuck, h)
		}
	}
	assert.Equal(t, []string{"stuck:0", "stuck:1", "stuck:2", "stuck:3"}, stuck,
		"what was queued is delivered in order; what was dropped never is")
}

func TestSessionLanes_WaitCtxBoundedByContext(t *testing.T) {
	release := make(chan struct{})
	lanes := newSessionLanes(context.Background(), 1, 0, func(*nats.Msg) { <-release })
	lanes.dispatch("s", laneMsg("s", 0))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, lanes.waitCtx(ctx), context.DeadlineExceeded, "a busy lane outlasting ctx reports ctx's error")

	close(release)
	assert.NoError(t, lanes.waitCtx(context.Background()), "and returns cleanly once the lane drains")
}

// Shutdown cancels the process's root context — the one the relay was started
// with — before Stop runs. A queued or in-flight delivery must not be cut off
// by that: it is still owed to the user, and Stop's own context is what bounds
// how long shutdown waits for it.
func TestRelay_RootContextCancelDoesNotCutOffDelivery(t *testing.T) {
	nc := connectNATS(t)
	cli := fakeClientWith(t, defaultSession())
	sndr := &gatedSender{entered: make(chan struct{}), release: make(chan struct{})}
	r := &Relay{NC: nc, K8s: cli, Senders: &fixedResolver{s: sndr}}
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	require.NoError(t, r.Start(rootCtx), "Relay.Start")

	publishOut(t, nc, "default", "foo", string(channelevents.KindUserMessage),
		buildEnv(t, "foo", channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "hi"}))
	<-sndr.entered

	cancelRoot() // SIGTERM: the root context goes first
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopped <- r.Stop(ctx)
	}()
	close(sndr.release)

	require.NoError(t, <-stopped, "Stop drains within its own grace period")
	assert.NoError(t, sndr.ctxErr, "the delivery's context outlived the root context's cancellation")
}

// gatedSender blocks its first Send until released, then records whether the
// context it was handed had been cancelled by then.
type gatedSender struct {
	entered, release chan struct{}
	once             sync.Once
	ctxErr           error
}

func (s *gatedSender) Send(ctx context.Context, _ channelkinds.SessionInfo, _ channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	first := false
	s.once.Do(func() { first = true; close(s.entered) })
	if first {
		<-s.release
		s.ctxErr = ctx.Err()
	}
	return channelkinds.SubChannelSendResult{}, ctx.Err()
}
