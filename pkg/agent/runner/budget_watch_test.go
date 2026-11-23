package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestBudget_WatchDuration(t *testing.T) {
	durCfg := func(d time.Duration) spiceboxv1alpha1.BudgetConfig {
		return spiceboxv1alpha1.BudgetConfig{MaxDuration: metav1.Duration{Duration: d}}
	}

	t.Run("run clock reaches the cap: onExceeded fires", func(t *testing.T) {
		clock := NewRunClock(0, time.Now)
		b := NewBudget(durCfg(20*time.Millisecond), clock, time.Time{})
		fired := make(chan struct{})
		go b.watchDuration(t.Context(), func() { close(fired) })
		select {
		case <-fired:
		case <-time.After(5 * time.Second):
			t.Fatal("watchDuration did not fire after the run clock passed the cap")
		}
	})

	t.Run("seeded past the cap: fires immediately", func(t *testing.T) {
		b := NewBudget(durCfg(time.Minute), NewRunClock(2*time.Minute, time.Now), time.Time{})
		fired := false
		b.watchDuration(t.Context(), func() { fired = true })
		assert.True(t, fired, "a budget already spent at pod start fires without waiting")
	})

	t.Run("clock paused below the cap: does not fire while parked", func(t *testing.T) {
		clock := NewRunClock(0, time.Now)
		clock.Pause()
		b := NewBudget(durCfg(10*time.Millisecond), clock, time.Time{})
		ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
		defer cancel()
		fired := false
		b.watchDuration(ctx, func() { fired = true })
		assert.False(t, fired, "time parked on a human is not run-time, so the cap is not reached")
	})

	t.Run("no maxDuration or no clock: returns without firing", func(t *testing.T) {
		fired := false
		NewBudget(durCfg(0), NewRunClock(time.Hour, time.Now), time.Time{}).watchDuration(t.Context(), func() { fired = true })
		NewBudget(durCfg(time.Minute), nil, time.Time{}).watchDuration(t.Context(), func() { fired = true })
		var nilBudget *Budget
		nilBudget.watchDuration(t.Context(), func() { fired = true })
		assert.False(t, fired)
	})
}
