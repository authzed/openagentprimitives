package toolguard

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock advances manually.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func testRule() ResolvedRule {
	return ResolvedRule{
		FailureThreshold:       3,
		OriginFailureThreshold: 5,
		InitialCoolOff:         30 * time.Second,
		MaxCoolOff:             2 * time.Minute,
		Action:                 ActionDeny,
		RateAction:             ActionDeny,
	}
}

func TestBreakerLifecycle(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewRegistry(clk.now)
	rule := testRule()
	tk, ok := "tool/flaky", "origin/mcpserver/github"

	// Closed: failures below threshold keep admitting.
	for i := 0; i < 2; i++ {
		adm, _ := r.Admit(context.Background(), tk, ok, rule, 0)
		require.True(t, adm.Allowed)
		r.Record(tk, ok, rule, true)
	}
	// Third consecutive failure trips the tool breaker.
	adm, _ := r.Admit(context.Background(), tk, ok, rule, 0)
	require.True(t, adm.Allowed)
	trans := r.Record(tk, ok, rule, true)
	require.Len(t, trans, 1)
	assert.Equal(t, "breaker_opened", trans[0].Event)
	assert.Equal(t, tk, trans[0].Key)
	assert.Equal(t, int32(1), trans[0].Trips)
	assert.Equal(t, 30*time.Second, trans[0].CoolOff)

	// Open: denied with remaining cool-off.
	adm, _ = r.Admit(context.Background(), tk, ok, rule, 0)
	assert.False(t, adm.Allowed)
	assert.Equal(t, "breaker", adm.DeniedBy)
	assert.Equal(t, tk, adm.Key)
	assert.Equal(t, 30*time.Second, adm.RetryAfter)

	// Cool-off elapsed → half-open, exactly one probe.
	clk.t = clk.t.Add(31 * time.Second)
	adm, trans = r.Admit(context.Background(), tk, ok, rule, 0)
	require.True(t, adm.Allowed)
	assert.True(t, adm.Probe)
	require.Len(t, trans, 1)
	assert.Equal(t, "breaker_half_open", trans[0].Event)
	// Second concurrent call during the probe: denied.
	adm2, _ := r.Admit(context.Background(), tk, ok, rule, 0)
	assert.False(t, adm2.Allowed)
	assert.Equal(t, "probe", adm2.DeniedBy)

	// Probe fails → re-open with doubled cool-off.
	trans = r.Record(tk, ok, rule, true)
	require.Len(t, trans, 1)
	assert.Equal(t, "breaker_opened", trans[0].Event)
	assert.Equal(t, int32(2), trans[0].Trips)
	assert.Equal(t, 60*time.Second, trans[0].CoolOff)

	// Next probe succeeds → closed, trip count reset.
	clk.t = clk.t.Add(61 * time.Second)
	adm, _ = r.Admit(context.Background(), tk, ok, rule, 0)
	require.True(t, adm.Allowed)
	trans = r.Record(tk, ok, rule, false)
	require.Len(t, trans, 1)
	assert.Equal(t, "breaker_closed", trans[0].Event)
	// Trip count reset: a fresh trip starts back at the initial cool-off.
	for i := 0; i < 3; i++ {
		_, _ = r.Admit(context.Background(), tk, ok, rule, 0)
		trans = r.Record(tk, ok, rule, true)
	}
	require.Len(t, trans, 1)
	assert.Equal(t, int32(1), trans[0].Trips)
	assert.Equal(t, 30*time.Second, trans[0].CoolOff)
}

func TestBackoffCapsAtMaxCoolOff(t *testing.T) {
	cases := []struct {
		name  string
		trips int32
		want  time.Duration
	}{
		{name: "trip 1 → initial 30s", trips: 1, want: 30 * time.Second},
		{name: "trip 2 → 60s", trips: 2, want: 60 * time.Second},
		{name: "trip 3 → 120s = cap", trips: 3, want: 2 * time.Minute},
		{name: "trip 4 → stays at cap", trips: 4, want: 2 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, backoff(30*time.Second, 2*time.Minute, tc.trips))
		})
	}
}

func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewRegistry(clk.now)
	rule := testRule()
	// 2 failures, success, 2 failures: never trips (threshold 3 consecutive).
	for _, failed := range []bool{true, true, false, true, true} {
		adm, _ := r.Admit(context.Background(), "tool/x", "", rule, 0)
		require.True(t, adm.Allowed)
		trans := r.Record("tool/x", "", rule, failed)
		assert.Empty(t, trans)
	}
}

func TestOriginBreakerTripsAcrossSiblingsAndDeniesAll(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewRegistry(clk.now)
	rule := testRule() // origin threshold 5
	ok := "origin/mcpserver/github"
	// 5 consecutive failures spread across different tools of one origin.
	// Tool-level keys differ so no tool breaker trips (threshold 3 per tool,
	// max 2 fails on tool/a + 2 on tool/b + 1 on tool/c).
	tools := []string{"tool/a", "tool/a", "tool/b", "tool/b", "tool/c"}
	var lastTrans []Transition
	for _, tk := range tools {
		adm, _ := r.Admit(context.Background(), tk, ok, rule, 0)
		require.True(t, adm.Allowed, "tool %s should admit", tk)
		lastTrans = r.Record(tk, ok, rule, true)
	}
	require.Len(t, lastTrans, 1)
	assert.Equal(t, "breaker_opened", lastTrans[0].Event)
	assert.Equal(t, ok, lastTrans[0].Key)

	// Every sibling — even a never-failed tool — is now denied by the origin.
	adm, _ := r.Admit(context.Background(), "tool/never-failed", ok, rule, 0)
	assert.False(t, adm.Allowed)
	assert.Equal(t, ok, adm.Key)
}

func TestSiblingSuccessResetsOriginCounter(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewRegistry(clk.now)
	rule := testRule()
	ok := "origin/mcpserver/github"
	for i := 0; i < 4; i++ {
		_, _ = r.Admit(context.Background(), "tool/a", ok, rule, 0)
		r.Record("tool/a", ok, rule, true)
	}
	// Sibling success resets origin count.
	_, _ = r.Admit(context.Background(), "tool/b", ok, rule, 0)
	r.Record("tool/b", ok, rule, false)
	// 4 more failures spread across fresh tools (each below its own
	// threshold): origin count reaches 4 < 5, never opens.
	for _, tk := range []string{"tool/c", "tool/d", "tool/e", "tool/f"} {
		adm, _ := r.Admit(context.Background(), tk, ok, rule, 0)
		require.True(t, adm.Allowed)
		trans := r.Record(tk, ok, rule, true)
		for _, tr := range trans {
			assert.NotEqual(t, ok, tr.Key, "origin must not open")
		}
	}
}

func TestRateLimitPerTurn(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewRegistry(clk.now)
	rule := ResolvedRule{RateMaxPerTurn: 2, Action: ActionDeny, RateAction: ActionDeny}
	for i := 0; i < 2; i++ {
		adm, _ := r.Admit(context.Background(), "tool/x", "", rule, 7)
		require.True(t, adm.Allowed)
	}
	adm, _ := r.Admit(context.Background(), "tool/x", "", rule, 7)
	assert.False(t, adm.Allowed)
	assert.Equal(t, "rate_turn", adm.DeniedBy)
	// New turn resets the counter.
	adm, _ = r.Admit(context.Background(), "tool/x", "", rule, 8)
	assert.True(t, adm.Allowed)
}

func TestRateLimitSlidingWindow(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewRegistry(clk.now)
	rule := ResolvedRule{RateMaxCalls: 2, RateWindow: time.Minute, Action: ActionDeny, RateAction: ActionDeny}
	for i := 0; i < 2; i++ {
		adm, _ := r.Admit(context.Background(), "tool/x", "", rule, 0)
		require.True(t, adm.Allowed)
		clk.t = clk.t.Add(10 * time.Second)
	}
	adm, _ := r.Admit(context.Background(), "tool/x", "", rule, 0)
	assert.False(t, adm.Allowed)
	assert.Equal(t, "rate_window", adm.DeniedBy)
	// First call ages out of the window → admitted again.
	clk.t = clk.t.Add(45 * time.Second)
	adm, _ = r.Admit(context.Background(), "tool/x", "", rule, 0)
	assert.True(t, adm.Allowed)
}

func TestDenialDoesNotConsumeRateSlot(t *testing.T) {
	// Two-phase Admit: a breaker denial must not burn a window slot.
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewRegistry(clk.now)
	rule := ResolvedRule{
		FailureThreshold: 1, InitialCoolOff: time.Minute, MaxCoolOff: time.Minute,
		RateMaxCalls: 1, RateWindow: time.Hour,
		Action: ActionDeny, RateAction: ActionDeny,
	}
	_, _ = r.Admit(context.Background(), "tool/x", "", rule, 0)
	r.Record("tool/x", "", rule, true) // breaker opens (threshold 1); 1 window slot used
	for i := 0; i < 3; i++ {
		adm, _ := r.Admit(context.Background(), "tool/x", "", rule, 0)
		require.False(t, adm.Allowed)
		assert.Equal(t, "breaker", adm.DeniedBy, "breaker denial, not rate")
	}
}

func TestHalfOpenSingleProbeUnderConcurrency(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewRegistry(clk.now)
	rule := testRule()
	for i := 0; i < 3; i++ {
		_, _ = r.Admit(context.Background(), "tool/x", "", rule, 0)
		r.Record("tool/x", "", rule, true)
	}
	clk.t = clk.t.Add(time.Minute)

	const n = 16
	probes := make(chan bool, n)
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			adm, _ := r.Admit(context.Background(), "tool/x", "", rule, 0)
			if adm.Allowed {
				probes <- adm.Probe
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	close(probes)
	count := 0
	for p := range probes {
		require.True(t, p, "any admitted call during half-open must be THE probe")
		count++
	}
	assert.Equal(t, 1, count, "exactly one probe admitted")
}

func TestOpenBreakersSnapshot(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r := NewRegistry(clk.now)
	rule := testRule()
	for i := 0; i < 3; i++ {
		_, _ = r.Admit(context.Background(), "tool/x", "", rule, 0)
		r.Record("tool/x", "", rule, true)
	}
	snap := r.OpenBreakers()
	require.Len(t, snap, 1)
	assert.Equal(t, "tool/x", snap[0].Key)
	assert.Equal(t, int32(1), snap[0].Trips)
	assert.Equal(t, clk.t.Add(30*time.Second), snap[0].RetryAt)
}

func TestResolvedRuleDataLimitHelpers(t *testing.T) {
	cases := []struct {
		name     string
		rule     ResolvedRule
		hasData  bool
		disabled bool
	}{
		{"no limits, action off: disabled", ResolvedRule{Action: ActionOff}, false, true},
		{"egress only: has data, not disabled", ResolvedRule{Action: ActionOff, MaxEgressBytes: 100, ByteAction: ActionDeny}, true, false},
		{"ingress only: has data, not disabled", ResolvedRule{Action: ActionOff, MaxIngressBytes: 100, ByteAction: ActionDeny}, true, false},
		{"breaker on, no bytes: not disabled, no data", ResolvedRule{Action: ActionDeny, FailureThreshold: 5}, false, false},
		{"rate on, no bytes: not disabled, no data", ResolvedRule{Action: ActionOff, RateMaxPerTurn: 3, RateAction: ActionDeny}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.hasData, tc.rule.HasDataLimit())
			assert.Equal(t, tc.disabled, tc.rule.Disabled())
		})
	}
}
