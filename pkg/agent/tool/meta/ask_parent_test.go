package meta

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	testclock "k8s.io/utils/clock/testing"
)

// askRecorder captures what ask_parent recorded and in what order relative to
// the yield, which is the property several of these tests turn on.
type askRecorder struct {
	mu        sync.Mutex
	questions []string
	events    []string // "record", "yield", "resume", in call order
	err       error
}

func (a *askRecorder) record(_ context.Context, q string) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "record")
	if a.err != nil {
		return 0, a.err
	}
	a.questions = append(a.questions, q)
	return int64(len(a.questions)), nil
}

func (a *askRecorder) note(what string) func(context.Context) {
	return func(context.Context) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.events = append(a.events, what)
	}
}

func (a *askRecorder) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.events...)
}

// Brief test 2 (child half): an answer wakes ask_parent, and the result marks
// the resume so the loop splices the reply in as the next turn — the same
// contract await_user_message has, because a child that could not see the
// answer would have parked for nothing.
func TestAskParent_AnInboundAnswerResumesTheChildAndFlagsTheDrain(t *testing.T) {
	rec := &askRecorder{}
	inbound := make(chan struct{}, 1)
	tl := NewAskParent(AskParentConfig{
		Record: rec.record,
		Await: AwaitConfig{
			IdleTTL:   time.Hour,
			InboundCh: inbound,
			OnYield:   rec.note("yield"),
			OnResume:  rec.note("resume"),
		},
	})

	done := make(chan struct{})
	var res struct {
		content      string
		awaitResumed bool
		terminal     bool
	}
	go func() {
		defer close(done)
		r, err := tl.Execute(context.Background(), json.RawMessage(`{"question":"which repo?"}`), nil)
		assert.NoError(t, err)
		res.content, res.awaitResumed, res.terminal = r.Content, r.AwaitResumed, r.Terminal
	}()

	// The question must be recorded BEFORE the tool parks: the parent learns of
	// it only through that write, so a yield that preceded it would wait in
	// silence.
	require.Eventually(t, func() bool {
		e := rec.seen()
		return len(e) >= 2 && e[0] == "record" && e[1] == "yield"
	}, 2*time.Second, 5*time.Millisecond, "record then yield, in that order")

	inbound <- struct{}{}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ask_parent did not return after an inbound answer arrived")
	}

	assert.True(t, res.awaitResumed, "the loop drains the held inbox only on this flag; without it the answer is stranded")
	assert.False(t, res.terminal, "an answered question continues the turn, it does not end the session")
	assert.Equal(t, []string{"record", "yield", "resume"}, rec.seen())
	assert.Equal(t, []string{"which repo?"}, rec.questions)
}

// Brief test 4: the park does not burn run-time. The tool cannot pause the run
// clock itself — OnYield is what does — so the property to hold here is that
// it ALWAYS yields before blocking. pkg/agent/runner's own test pins the other
// half (that OnAwaitYield stops the clock).
//
// Drop the OnYield call from yieldAndWait and this fails; no content assertion
// anywhere else in this package would.
func TestAskParent_YieldsBeforeBlocking_SoParkedTimeIsNotRunTime(t *testing.T) {
	rec := &askRecorder{}
	yielded := make(chan struct{})
	var once sync.Once
	tl := NewAskParent(AskParentConfig{
		Record: rec.record,
		Await: AwaitConfig{
			IdleTTL:   time.Hour,
			InboundCh: make(chan struct{}), // never fires
			OnYield:   func(context.Context) { once.Do(func() { close(yielded) }) },
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_, _ = tl.Execute(ctx, json.RawMessage(`{"question":"q"}`), nil)
	}()

	select {
	case <-yielded:
	case <-time.After(2 * time.Second):
		t.Fatal("ask_parent blocked without yielding: the run clock would keep accruing for the whole wait")
	}
}

// Brief test 5 (child half): the idle TTL parks the child rather than ending
// the delegation. The question STAYS outstanding, so a later reply still wakes
// it — the bound that actually ends an unanswered delegation is the
// SubagentRequest controller's, tested there.
func TestAskParent_IdleTTLExpiry_ParksToIdleWithoutResuming(t *testing.T) {
	rec := &askRecorder{}
	clk := testclock.NewFakeClock(time.Now())
	tl := NewAskParent(AskParentConfig{
		Record: rec.record,
		Await: AwaitConfig{
			IdleTTL:   time.Minute,
			InboundCh: make(chan struct{}),
			Clock:     clk,
			OnYield:   rec.note("yield"),
			OnResume:  rec.note("resume"),
		},
	})

	type out struct {
		content            string
		terminal, idleExit bool
		awaitResumed       bool
	}
	got := make(chan out, 1)
	go func() {
		r, err := tl.Execute(context.Background(), json.RawMessage(`{"question":"q"}`), nil)
		assert.NoError(t, err)
		got <- out{r.Content, r.Terminal, r.IdleExit, r.AwaitResumed}
	}()

	require.Eventually(t, func() bool { return clk.HasWaiters() }, 2*time.Second, 5*time.Millisecond,
		"the tool must be parked on the idle timer before it can be expired")
	clk.Step(2 * time.Minute)

	select {
	case r := <-got:
		assert.True(t, r.terminal)
		assert.True(t, r.idleExit, "the pod exits to phase=Idle; the parent's reply re-hydrates it")
		assert.False(t, r.awaitResumed, "nobody answered, so there is nothing to drain")
		assert.Contains(t, r.content, "parking")
	case <-time.After(2 * time.Second):
		t.Fatal("ask_parent did not return after the idle TTL expired")
	}
	assert.NotContains(t, rec.seen(), "resume", "a TTL expiry is not a resume")
}

func TestAskParent_RefusesWhatItCannotAsk(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		recErr  error
		record  func(context.Context, string) (int64, error)
		wantErr string
	}{
		{
			name:    "an empty question: refused before anything is recorded",
			args:    `{"question":"   "}`,
			wantErr: "question must not be empty",
		},
		{
			name:    "a question past the status field's own bound: refused, never truncated",
			args:    `{"question":"` + strings.Repeat("x", maxQuestionRunes+1) + `"}`,
			wantErr: "too long",
		},
		{
			name:    "nothing wired to carry it: refused rather than parked in silence",
			args:    `{"question":"q"}`,
			record:  nil, // explicit: no Record func at all
			wantErr: "not available in this session",
		},
		{
			name:    "the status write failed: the child is told, and told not to wait",
			args:    `{"question":"q"}`,
			recErr:  assert.AnError,
			wantErr: "will never see it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := AskParentConfig{Await: AwaitConfig{IdleTTL: time.Hour, InboundCh: make(chan struct{})}}
			// The "nothing wired" row is the one case that must leave Record
			// nil; every other row needs a working (or failing) recorder.
			if !strings.Contains(tc.name, "nothing wired") {
				rec := &askRecorder{err: tc.recErr}
				cfg.Record = rec.record
			}
			res, err := NewAskParent(cfg).Execute(context.Background(), json.RawMessage(tc.args), nil)
			require.NoError(t, err)
			assert.True(t, res.IsError)
			assert.True(t, res.Trusted, "the tool's own refusal is platform-authored")
			assert.False(t, res.Terminal, "a refused question leaves the session running, free to carry on")
			assert.Contains(t, res.Content, tc.wantErr)
		})
	}
}

// Brief test 2 (child half): a question the delegation has no budget left for
// comes back as a legible tool result IN THE SAME TURN. The three properties
// that make this the right refusal are asserted separately, because each rules
// out a different wrong answer:
//
//   - it never parks (no yield) — the child is not left waiting on an answer
//     that is not coming;
//   - it is not Terminal and not IdleExit — nothing is torn down, and the model
//     has the rest of this turn to act;
//   - the words say what was spent and what to do instead — "refused" alone
//     invites a retry, and every retry is refused the same way.
func TestAskParent_BudgetSpent_RefusesInTheSameTurnWithoutParking(t *testing.T) {
	rec := &askRecorder{err: ErrExchangeBudgetSpent}
	tl := NewAskParent(AskParentConfig{
		Record: rec.record,
		Await: AwaitConfig{
			IdleTTL:   time.Hour,
			InboundCh: make(chan struct{}), // nothing will ever arrive on it
			OnYield:   rec.note("yield"),
			OnResume:  rec.note("resume"),
		},
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"question":"and which branch?"}`), nil)
	require.NoError(t, err, "a refusal is a RESULT, not a tool failure")

	assert.Equal(t, []string{"record"}, rec.seen(),
		"the tool must return without yielding: a park here waits out the whole idle TTL for an answer nobody will send")
	assert.True(t, res.IsError, "the model has to see this as a refused call, not as an answer")
	assert.True(t, res.Trusted, "the platform authored this refusal")
	assert.False(t, res.Terminal, "the child keeps working; a refused question is not the end of the delegation")
	assert.False(t, res.IdleExit, "nothing about a refusal sends the session to Idle")
	assert.Contains(t, res.Content, "budget", "the child must be told WHAT it ran out of")
	assert.Contains(t, res.Content, "Do not ask again", "without this the model retries, and every retry is refused")
	assert.Contains(t, res.Content, "finish now", "it needs somewhere to go, or it stalls having been told only what it cannot do")
	assert.Empty(t, rec.questions, "a refused question is recorded nowhere")
}

func TestAskParent_ExchangeNumbersComeFromTheRecorderAndAdvance(t *testing.T) {
	rec := &askRecorder{}
	inbound := make(chan struct{}, 2)
	tl := NewAskParent(AskParentConfig{
		Record: rec.record,
		Await:  AwaitConfig{IdleTTL: time.Hour, InboundCh: inbound},
	})
	for i := 0; i < 2; i++ {
		inbound <- struct{}{}
		_, err := tl.Execute(context.Background(), json.RawMessage(`{"question":"q"}`), nil)
		require.NoError(t, err)
	}
	assert.Len(t, rec.questions, 2, "each ask records its own exchange; the recorder is what numbers them")
}
