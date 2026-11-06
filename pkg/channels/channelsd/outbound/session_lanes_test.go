package outbound

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func laneMsg(key string, i int) *nats.Msg {
	return &nats.Msg{Subject: key, Data: []byte(fmt.Sprint(i))}
}

// The failure this exists to prevent: one session's slow delivery held every
// other session's output behind it, for twenty minutes on a live cluster.
func TestSessionLanes_BlockedSessionDoesNotHoldUpOthers(t *testing.T) {
	release := make(chan struct{})
	delivered := make(chan string, 1)
	lanes := newSessionLanes(context.Background(), 4, func(m *nats.Msg) {
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
	lanes := newSessionLanes(context.Background(), 3, func(m *nats.Msg) {
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
	lanes := newSessionLanes(context.Background(), limit, func(*nats.Msg) {
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
	lanes := newSessionLanes(context.Background(), 2, func(*nats.Msg) {})
	lanes.dispatch("s", laneMsg("s", 0))
	lanes.wait()

	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	assert.Empty(t, lanes.lanes, "a drained lane is removed, so idle sessions hold no goroutine or entry")
}
