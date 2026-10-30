package linkinstall

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGate(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, g *Gate)
	}{
		{"ready and idle: RunNow", func(t *testing.T, g *Gate) {
			assert.Equal(t, RunNow, g.Offer("a", Ready))
		}},
		{"ready twice: RunNow then RejectBusy", func(t *testing.T, g *Gate) {
			d := g.Offer("a", Ready)
			assert.Equal(t, RunNow, d)
			d = g.Offer("b", Ready)
			assert.Equal(t, RejectBusy, d)
		}},
		{"ready, Release, ready: RunNow both times", func(t *testing.T, g *Gate) {
			d := g.Offer("a", Ready)
			assert.Equal(t, RunNow, d)
			g.Release()
			d = g.Offer("b", Ready)
			assert.Equal(t, RunNow, d)
		}},
		{"starting twice: Held then RejectPending, first link kept", func(t *testing.T, g *Gate) {
			assert.Equal(t, Held, g.Offer("a", Starting))
			assert.Equal(t, RejectPending, g.Offer("b", Starting))
			l, ok := g.TakePending()
			assert.True(t, ok)
			assert.Equal(t, "a", l)
		}},
		{"starting after DropPending: Held again", func(t *testing.T, g *Gate) {
			g.Offer("a", Starting)
			g.DropPending()
			assert.Equal(t, Held, g.Offer("b", Starting))
		}},
		{"starting, TakePending, ready: held taken, then RejectBusy", func(t *testing.T, g *Gate) {
			g.Offer("a", Starting)
			l, ok := g.TakePending()
			assert.True(t, ok)
			assert.Equal(t, "a", l)
			d := g.Offer("b", Ready)
			assert.Equal(t, RejectBusy, d)
		}},
		{"starting, DropPending: returns held, then TakePending finds nothing", func(t *testing.T, g *Gate) {
			g.Offer("a", Starting)
			l, ok := g.DropPending()
			assert.True(t, ok)
			assert.Equal(t, "a", l)
			l, ok = g.TakePending()
			assert.False(t, ok)
			assert.Empty(t, l)
		}},
		{"unavailable: RejectUnavailable and nothing held", func(t *testing.T, g *Gate) {
			d := g.Offer("a", Unavailable)
			assert.Equal(t, RejectUnavailable, d)
			_, ok := g.TakePending()
			assert.False(t, ok)
		}},
		{"busy while starting: RejectBusy wins over Held", func(t *testing.T, g *Gate) {
			g.Offer("a", Ready)
			d := g.Offer("b", Starting)
			assert.Equal(t, RejectBusy, d)
			_, ok := g.DropPending()
			assert.False(t, ok)
		}},
		{"unavailable leaves a held link untouched", func(t *testing.T, g *Gate) {
			g.Offer("a", Starting)
			d := g.Offer("b", Unavailable)
			assert.Equal(t, RejectUnavailable, d)
			l, ok := g.DropPending()
			assert.True(t, ok)
			assert.Equal(t, "a", l)
		}},
		{"TakePending while busy: nothing taken, held link kept", func(t *testing.T, g *Gate) {
			g.Offer("a", Starting)
			g.Offer("b", Ready) // idle, so RunNow; busy set
			_, ok := g.TakePending()
			assert.False(t, ok)
			l, ok := g.DropPending()
			assert.True(t, ok)
			assert.Equal(t, "a", l)
		}},
		{"DropPending leaves busy untouched", func(t *testing.T, g *Gate) {
			g.Offer("a", Ready)
			g.DropPending()
			d := g.Offer("b", Ready)
			assert.Equal(t, RejectBusy, d)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, &Gate{}) })
	}
}

func TestGateConcurrentReadyOffersAdmitExactlyOne(t *testing.T) {
	var g Gate
	var runNow atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d := g.Offer("x", Ready); d == RunNow {
				runNow.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), runNow.Load())
}

func TestStringers(t *testing.T) {
	assert.Equal(t, "ready", Ready.String())
	assert.Equal(t, "starting", Starting.String())
	assert.Equal(t, "unavailable", Unavailable.String())
	assert.Equal(t, "run-now", RunNow.String())
	assert.Equal(t, "held", Held.String())
	assert.Equal(t, "reject-unavailable", RejectUnavailable.String())
	assert.Equal(t, "reject-busy", RejectBusy.String())
	assert.Equal(t, "reject-pending", RejectPending.String())
	assert.Equal(t, "reject-cooling-down", RejectCoolingDown.String())
}

func TestGateCoolDown(t *testing.T) {
	now := time.Unix(1_000, 0)
	g := &Gate{Now: func() time.Time { return now }}

	g.CoolDown(10 * time.Second)
	assert.Equal(t, RejectCoolingDown, g.Offer("a", Ready), "ready during cooldown")
	assert.Equal(t, RejectCoolingDown, g.Offer("a", Starting), "starting during cooldown: nothing held")
	_, held := g.Pending()
	assert.False(t, held)

	g.CoolDown(time.Second)
	now = now.Add(5 * time.Second)
	assert.Equal(t, RejectCoolingDown, g.Offer("a", Ready), "a shorter CoolDown never shortens a running one")

	now = now.Add(5 * time.Second)
	assert.Equal(t, RunNow, g.Offer("a", Ready), "cooldown over")

	g.CoolDown(time.Minute)
	assert.Equal(t, RejectBusy, g.Offer("b", Ready), "busy wins over cooldown")
}

func TestGatePendingPeeksWithoutClaiming(t *testing.T) {
	var g Gate
	_, ok := g.Pending()
	assert.False(t, ok, "empty gate: nothing pending")

	g.Offer("held", Starting)
	g.Offer("other", Ready) // busy now, with "held" still pending
	got, ok := g.Pending()
	assert.True(t, ok)
	assert.Equal(t, "held", got)
	_, took := g.TakePending()
	assert.False(t, took, "Pending must not claim or clear: busy still blocks TakePending")
	got, ok = g.Pending()
	assert.True(t, ok)
	assert.Equal(t, "held", got, "still held after the peek")
}
