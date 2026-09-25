package progress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
)

type fakeSink struct {
	mu               sync.Mutex // guards warns and warnMessages; the ETA goroutine calls warn concurrently
	interactiveV     bool
	answers          []bool   // keepWaiting returns these in order, then false
	warns            int      // total warn calls
	warnMessages     []string // message text of each warn call, for content assertions
	diagnoses        int
	keepWaitingCalls int
	fix              FixFunc // when set, fixHook() returns it (enables the [f] path)
	onFixNonNil      int     // number of keepWaiting calls that received a non-nil onFix
}

func (f *fakeSink) onPoll(string, time.Duration, time.Duration, time.Duration) {}
func (f *fakeSink) tick(string, time.Duration, time.Duration, time.Duration)   {}
func (f *fakeSink) animated() bool                                             { return false }
func (f *fakeSink) warn(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warns++
	f.warnMessages = append(f.warnMessages, fmt.Sprintf(format, a...))
}
func (f *fakeSink) renderDiagnosis(wait.Diagnosis) { f.diagnoses++ }
func (f *fakeSink) interactive() bool              { return f.interactiveV }
func (f *fakeSink) fixHook() FixFunc               { return f.fix }
func (f *fakeSink) keepWaiting(_ string, onFix func() error) bool {
	f.keepWaitingCalls++
	if onFix != nil {
		f.onFixNonNil++
	}
	if len(f.answers) == 0 {
		return false
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a
}

const tinyDeadline = 25 * time.Millisecond

func TestAwaitLoop_ReadyOnFirstPoll_NoWait(t *testing.T) {
	err := awaitLoop(context.Background(), context.Background(), &fakeSink{}, "postgres", time.Second,
		func(context.Context) (bool, error) { return true, nil }, nil, false, 0)
	require.NoError(t, err)
}

func TestAwaitLoop_PollError_Propagates(t *testing.T) {
	want := errors.New("boom")
	err := awaitLoop(context.Background(), context.Background(), &fakeSink{}, "postgres", time.Second,
		func(context.Context) (bool, error) { return false, want }, nil, false, 0)
	require.ErrorIs(t, err, want)
}

func TestAwaitLoop_NonInteractiveTimeout_DiagnosesAndAborts(t *testing.T) {
	f := &fakeSink{interactiveV: false}
	diag := func(context.Context) (wait.Diagnosis, error) { return wait.Diagnosis{Pod: "p"}, nil }
	err := awaitLoop(context.Background(), context.Background(), f, "postgres", tinyDeadline,
		func(context.Context) (bool, error) { return false, nil }, diag, false, 0)
	require.Error(t, err)
	assert.Equal(t, 1, f.diagnoses, "diagnosed once before aborting")
	assert.Equal(t, 1, f.warns)
}

func TestAwaitLoop_InteractiveKeepWaiting_LoopsThenStops(t *testing.T) {
	f := &fakeSink{interactiveV: true, answers: []bool{true, false}}
	diag := func(context.Context) (wait.Diagnosis, error) { return wait.Diagnosis{}, nil }
	// Use awaitLoopRecheck with tinyDeadline as recheckWindow so the keep-waiting
	// round also uses a tiny window instead of the full 30s keepWaitingRecheck const.
	err := awaitLoopRecheck(context.Background(), context.Background(), f, "postgres", tinyDeadline, tinyDeadline,
		func(context.Context) (bool, error) { return false, nil }, diag, false, 0)
	require.Error(t, err, "user eventually stopped waiting")
	assert.Equal(t, 2, f.diagnoses, "diagnosed each round")
	assert.Equal(t, 2, f.warns, "warned once per stall round")
}

func TestAwaitLoop_DiagnoseError_SurfacedNotSwallowed(t *testing.T) {
	f := &fakeSink{interactiveV: false}
	diag := func(context.Context) (wait.Diagnosis, error) { return wait.Diagnosis{}, errors.New("api down") }
	err := awaitLoop(context.Background(), context.Background(), f, "postgres", tinyDeadline,
		func(context.Context) (bool, error) { return false, nil }, diag, false, 0)
	require.Error(t, err)
	assert.GreaterOrEqual(t, f.warns, 2, "one warn for the stall, one for the diagnose failure")
}

func TestConfirmKeepWaiting(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"empty/Enter keeps waiting", "\n", true},
		{"y keeps waiting", "y\n", true},
		{"n stops", "n\n", false},
		{"EOF (closed stdin) stops", "", false},
		// With no fixer, 'f' is not an offered option → falls to the default (keep).
		{"f without fixer keeps waiting", "f\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := confirmKeepWaiting(strings.NewReader(tc.in), &bytes.Buffer{}, "postgres", nil)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestConfirmKeepWaiting_PromptShowsFWhenFixerSet asserts the prompt text adapts
// to whether an AI fixer is available: [Y/n/f] when set, [Y/n] when not.
func TestConfirmKeepWaiting_PromptShowsFWhenFixerSet(t *testing.T) {
	t.Run("fixer set: prompt offers [Y/n/f]", func(t *testing.T) {
		var out bytes.Buffer
		confirmKeepWaiting(strings.NewReader("n\n"), &out, "postgres", func() error { return nil })
		assert.Contains(t, out.String(), "[Y/n/f]")
		assert.Contains(t, out.String(), "AI assistant")
	})
	t.Run("no fixer: prompt stays [Y/n]", func(t *testing.T) {
		var out bytes.Buffer
		confirmKeepWaiting(strings.NewReader("n\n"), &out, "postgres", nil)
		assert.Contains(t, out.String(), "[Y/n]")
		assert.NotContains(t, out.String(), "[Y/n/f]")
	})
}

// TestConfirmKeepWaiting_FInvokesFixerThenReAsks scripts "f\nn\n": pressing 'f'
// must invoke onFix exactly once and then RE-ASK, where the user answers 'n' to
// stop. The fake onFix records its invocation.
func TestConfirmKeepWaiting_FInvokesFixerThenReAsks(t *testing.T) {
	var calls int
	onFix := func() error { calls++; return nil }
	var out bytes.Buffer
	got := confirmKeepWaiting(strings.NewReader("f\nn\n"), &out, "postgres", onFix)
	assert.False(t, got, "second answer 'n' must stop waiting")
	assert.Equal(t, 1, calls, "'f' must invoke the fixer exactly once before re-asking")
	// The prompt was shown twice (once before 'f', once on the re-ask).
	assert.GreaterOrEqual(t, strings.Count(out.String(), "Keep waiting for postgres?"), 2,
		"prompt must be re-asked after the fixer returns")
}

// TestConfirmKeepWaiting_FixerErrorSurfacedNotSwallowed asserts a Launch error is
// surfaced to the user (not silently dropped) and the prompt is still re-asked.
func TestConfirmKeepWaiting_FixerErrorSurfacedNotSwallowed(t *testing.T) {
	onFix := func() error { return errors.New("claude not found") }
	var out bytes.Buffer
	got := confirmKeepWaiting(strings.NewReader("f\ny\n"), &out, "postgres", onFix)
	assert.True(t, got, "after the failed fixer and a 'y', keep waiting")
	assert.Contains(t, out.String(), "AI fixer could not be launched")
	assert.Contains(t, out.String(), "claude not found")
}

// TestAwaitLoop_FixHookThreadsOnFixToKeepWaiting verifies the await loop passes a
// non-nil onFix to keepWaiting when a fix hook is set, and nil when it is not.
func TestAwaitLoop_FixHookThreadsOnFixToKeepWaiting(t *testing.T) {
	t.Run("fix hook set: onFix threaded", func(t *testing.T) {
		f := &fakeSink{interactiveV: true, answers: []bool{false}, fix: func(context.Context, string, wait.Diagnosis) error { return nil }}
		diag := func(context.Context) (wait.Diagnosis, error) { return wait.Diagnosis{Pod: "p"}, nil }
		_ = awaitLoopRecheck(context.Background(), context.Background(), f, "postgres", tinyDeadline, tinyDeadline,
			func(context.Context) (bool, error) { return false, nil }, diag, false, 0)
		assert.Equal(t, 1, f.onFixNonNil, "keepWaiting must receive a non-nil onFix when a fix hook is set")
	})
	t.Run("no fix hook: onFix nil", func(t *testing.T) {
		f := &fakeSink{interactiveV: true, answers: []bool{false}}
		diag := func(context.Context) (wait.Diagnosis, error) { return wait.Diagnosis{}, nil }
		_ = awaitLoopRecheck(context.Background(), context.Background(), f, "postgres", tinyDeadline, tinyDeadline,
			func(context.Context) (bool, error) { return false, nil }, diag, false, 0)
		assert.Equal(t, 0, f.onFixNonNil, "keepWaiting must receive a nil onFix when no fix hook is set")
	})
}

func TestAwaitLoop_Optional_NeverPrompts(t *testing.T) {
	f := &fakeSink{interactiveV: true, answers: []bool{true, true}}
	diag := func(context.Context) (wait.Diagnosis, error) { return wait.Diagnosis{}, nil }
	err := awaitLoop(context.Background(), context.Background(), f, "graphiti", tinyDeadline,
		func(context.Context) (bool, error) { return false, nil }, diag, true, 0)
	require.Error(t, err)
	assert.Equal(t, 0, f.keepWaitingCalls, "optional await must never prompt")
	assert.Equal(t, 1, f.diagnoses, "diagnosed once then returned")
}

func TestAwaitLoop_KeepWaiting_RechecksOnFreshWindow_NotBoundByExhaustedParent(t *testing.T) {
	// Parent ctx is ALREADY past its deadline — simulates an exhausted --timeout.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	// recheckBase is alive (Ctrl-C-cancelable, no timeout).
	recheck := context.Background()

	var polls int
	f := &fakeSink{interactiveV: true, answers: []bool{true, false}} // keep waiting once, then stop
	diag := func(context.Context) (wait.Diagnosis, error) { return wait.Diagnosis{}, nil }
	err := awaitLoopRecheck(expired, recheck, f, "gw", tinyDeadline, 40*time.Millisecond,
		func(context.Context) (bool, error) { polls++; return false, nil }, diag, false, 0)
	require.Error(t, err)
	// The keep-waiting round must actually have polled against the live recheck window,
	// not returned instantly under the expired parent.
	assert.GreaterOrEqual(t, polls, 2, "Y must buy a real recheck window of polling")
	assert.Equal(t, 2, f.keepWaitingCalls, "keepWaiting called once before recheck (Y) and once after the recheck window (N)")
}

// TestHumanETA covers the output of the humanETA helper across the two
// branches (≥ 1 minute rounded to minutes; < 1 minute rounded to seconds).
func TestHumanETA(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{7 * time.Minute, "7m"},
		{2 * time.Minute, "2m"},
		{90 * time.Second, "2m"},  // rounds to nearest minute → 2m, 0s trimmed
		{45 * time.Second, "45s"}, // < 1 minute → seconds
		{30 * time.Second, "30s"},
	}
	for _, tc := range cases {
		t.Run(tc.d.String(), func(t *testing.T) {
			assert.Equal(t, tc.want, humanETA(tc.d))
		})
	}
}

// TestETARender_Streaming_IncludesExpectedSuffix verifies that the streaming
// renderer appends ", ~<eta> expected" when eta > 0 and omits it when eta == 0.
func TestETARender_Streaming_IncludesExpectedSuffix(t *testing.T) {
	cases := []struct {
		name        string
		eta         time.Duration
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "eta present: suffix shown",
			eta:         7 * time.Minute,
			wantContain: "~7m expected",
		},
		{
			name:       "eta zero: suffix absent",
			eta:        0,
			wantAbsent: "expected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := newStreaming(&buf, strings.NewReader(""), false)
			ph := s.Phase("webd gateway address").(*streamingPhase)
			ph.onPoll("webd gateway address", 2*time.Second, 2*time.Second, tc.eta)

			out := buf.String()
			if tc.wantContain != "" {
				assert.Contains(t, out, tc.wantContain,
					"streaming onPoll must include ETA suffix when eta > 0")
			}
			if tc.wantAbsent != "" {
				assert.NotContains(t, out, tc.wantAbsent,
					"streaming onPoll must omit ETA suffix when eta == 0")
			}
		})
	}
}

// TestETARender_Checklist_IncludesDotSuffix verifies that the checklist
// renderer sets the row label to "waiting (<elapsed> · ~<eta>)" when eta > 0
// and "waiting (<elapsed>)" when eta == 0.
func TestETARender_Checklist_IncludesDotSuffix(t *testing.T) {
	cases := []struct {
		name        string
		eta         time.Duration
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "eta present: dot-eta suffix shown",
			eta:         7 * time.Minute,
			wantContain: "· ~7m",
		},
		{
			name:       "eta zero: dot-eta suffix absent",
			eta:        0,
			wantAbsent: "·",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := newChecklist(&buf, strings.NewReader(""), false)
			ph := c.Phase("webd gateway address").(*checklistPhase)
			ph.onPoll("webd gateway address", 2*time.Second, 2*time.Second, tc.eta)
			require.NoError(t, c.Close())

			out := buf.String()
			if tc.wantContain != "" {
				assert.Contains(t, out, tc.wantContain,
					"checklist onPoll must include ETA suffix when eta > 0")
			}
			if tc.wantAbsent != "" {
				assert.NotContains(t, out, tc.wantAbsent,
					"checklist onPoll must omit ETA suffix when eta == 0")
			}
		})
	}
}

// TestAwaitLoop_ETASoftWarn_FiresOnceWhenElapsed verifies that when an ETA is
// set and that duration elapses while the component is still not ready, exactly
// one "taking longer than expected" warn is emitted (and not repeated). The poll
// deadline is longer than the ETA so the warn fires mid-wait, before the hard
// timeout triggers the standard "still not ready" warn.
func TestAwaitLoop_ETASoftWarn_FiresOnceWhenElapsed(t *testing.T) {
	const eta = 30 * time.Millisecond
	const deadline = eta * 6 // deadline >> eta so the warn fires well before timeout

	f := &fakeSink{interactiveV: false}
	// optional=true: returns after deadline without interactive prompt, keeping
	// the test self-contained.
	err := awaitLoopRecheck(context.Background(), context.Background(), f,
		"postgres", deadline, tinyDeadline,
		func(context.Context) (bool, error) { return false, nil },
		nil, true /* optional */, eta)
	require.Error(t, err, "poll never ready: must error")

	// Exactly one message must mention the ETA soft-warn.
	var etaWarnCount int
	for _, msg := range f.warnMessages {
		if strings.Contains(msg, "taking longer than expected") {
			etaWarnCount++
		}
	}
	assert.Equal(t, 1, etaWarnCount,
		"ETA soft-warn must fire exactly once; all warns: %v", f.warnMessages)
}
