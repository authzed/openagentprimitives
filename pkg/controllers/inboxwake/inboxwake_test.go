package inboxwake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

type recorded struct {
	ns, name string
	env      channelevents.Envelope
}

// An idle session (no runner, wake-eligible) receives the line as its next
// "inbox" turn, gets the wake annotation, and one KindUserMessage nudge.
func TestNotify_AppendsLineAndWakesAnIdleSession(t *testing.T) {
	sess := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "builder"},
		Status:     v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseIdle},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sess).
		WithStatusSubresource(&v1.AgentSession{}).Build()
	mem := memory.NewLocal(inmem.NewBackend())
	var got []recorded
	publish := func(_ context.Context, ns, name string, env channelevents.Envelope) error {
		got = append(got, recorded{ns, name, env})
		return nil
	}
	now := func() time.Time { return time.Date(2026, 9, 13, 4, 0, 0, 0, time.UTC) }

	require.NoError(t, Notify(context.Background(), c, c, mem, publish, now, "ns", "builder", "The person started testing the agent."))

	turns, err := turn.ReadAll(memory.WithSystemApproval(context.Background(), "test"), mem, memory.Scope{Kind: "session", ID: "ns/builder"})
	require.NoError(t, err)
	require.Len(t, turns, 1)
	assert.Equal(t, "inbox", turns[0].Role)
	assert.Equal(t, "The person started testing the agent.", turns[0].Content[0].Text)
	assert.Equal(t, now(), turns[0].CreatedAt, "the turn must be stamped with the injected clock, not time.Now()")
	assert.Empty(t, turns[0].Author, "nobody sent this line, so it is attributed to nobody")
	assert.Empty(t, turns[0].Via, "the line came through no view")

	var after v1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "builder"}, &after))
	assert.NotEmpty(t, after.Annotations[v1.AnnotationWakeRequestedAt], "an idle session must be asked to wake")

	require.Len(t, got, 1)
	assert.Equal(t, channelevents.KindUserMessage, got[0].env.Kind)
	assert.Equal(t, "builder", got[0].name)
}

// A nil memory or a nil publisher is reported, never fatal: the caller's own
// write already succeeded and must not be undone by a notification.
func TestNotify_NilCollaboratorsAreReportedNotFatal(t *testing.T) {
	sess := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "builder"},
		Status:     v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseIdle},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sess).
		WithStatusSubresource(&v1.AgentSession{}).Build()
	var err error
	require.NotPanics(t, func() {
		err = Notify(context.Background(), c, c, nil, nil, time.Now, "ns", "builder", "The test ended.")
	})
	require.Error(t, err, "a missing memory must reach the caller, not just the log")
	assert.NotErrorIs(t, err, ErrSessionOver, "a live session is not over")
}

// A missing session is reported, never fatal.
func TestNotify_MissingSessionIsReported(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	mem := memory.NewLocal(inmem.NewBackend())
	var err error
	require.NotPanics(t, func() {
		err = Notify(context.Background(), c, c, mem, func(context.Context, string, string, channelevents.Envelope) error { return nil },
			time.Now, "ns", "gone", "The test ended.")
	})
	require.Error(t, err, "a session that is gone must reach the caller")
	assert.True(t, apierrors.IsNotFound(errors.Unwrap(err)), "the NotFound must survive the wrap so a caller can tell it apart")
}

// Whatever the agent is doing, the line lands — the "inbox" role shares no
// (index, role) key with anything the runner writes, so there is no state of
// a LIVE session in which appending to it is unsafe. The one refusal is a
// session that is over, which no runner will come back from to read it. The
// rows below are every answer the phase can give: a live runner mid-turn,
// a live runner parked on a question, a reaped-but-wakeable session, and the
// three finished phases.
func TestNotify_AppendsToEveryLiveSessionAndRefusesAFinishedOne(t *testing.T) {
	parked := metav1.NewTime(time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC))
	cases := []struct {
		name      string
		status    v1.AgentSessionStatus
		wantErr   error
		wantTurns int
		wantStamp bool
		wantPubs  int
	}{
		{
			name:      "mid-turn (Running, nothing awaited): appended + nudged, pod left alone",
			status:    v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning},
			wantTurns: 1,
			wantStamp: false, // a live runner is never respawned; the nudge alone reaches it
			wantPubs:  1,
		},
		{
			name:      "parked on a question (Running, awaitingUserInputSince set): appended + nudged, pod left alone",
			status:    v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning, AwaitingUserInputSince: ptr.To(parked)},
			wantTurns: 1,
			wantStamp: false,
			wantPubs:  1,
		},
		{
			name:      "idle (pod reaped): appended + wake stamped + nudged",
			status:    v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseIdle},
			wantTurns: 1,
			wantStamp: true,
			wantPubs:  1,
		},
		{
			name:      "finished (Failed): ErrSessionOver, no turn, no wake",
			status:    v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseFailed},
			wantErr:   ErrSessionOver,
			wantTurns: 0,
			wantStamp: false,
			wantPubs:  0,
		},
		{
			name:      "finished (Succeeded, never archive-swept): ErrSessionOver, no turn, no wake",
			status:    v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseSucceeded},
			wantErr:   ErrSessionOver,
			wantTurns: 0,
			wantStamp: false,
			wantPubs:  0,
		},
		{
			name:      "forensic hold (Held, only a human's release restarts it): ErrSessionOver, no turn, no wake",
			status:    v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseHeld},
			wantErr:   ErrSessionOver,
			wantTurns: 0,
			wantStamp: false,
			wantPubs:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &v1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "builder"},
				Status:     tc.status,
			}
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sess).
				WithStatusSubresource(&v1.AgentSession{}).Build()
			mem := memory.NewLocal(inmem.NewBackend())
			var got []recorded
			publish := func(_ context.Context, ns, name string, env channelevents.Envelope) error {
				got = append(got, recorded{ns, name, env})
				return nil
			}

			err := Notify(context.Background(), c, c, mem, publish, time.Now, "ns", "builder", "The test ended.")

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr, "Notify must report the refusal to its caller")
			} else {
				require.NoError(t, err)
			}

			turns, rerr := turn.ReadAll(memory.WithSystemApproval(context.Background(), "test"), mem, memory.Scope{Kind: "session", ID: "ns/builder"})
			require.NoError(t, rerr)
			assert.Len(t, turns, tc.wantTurns, "appended turns")
			for _, tn := range turns {
				assert.Equal(t, "inbox", tn.Role, "an out-of-band writer never takes the runner's own role")
			}

			var after v1.AgentSession
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "builder"}, &after))
			if tc.wantStamp {
				assert.NotEmpty(t, after.Annotations[v1.AnnotationWakeRequestedAt], "a reaped session must be asked to wake")
			} else {
				assert.Empty(t, after.Annotations[v1.AnnotationWakeRequestedAt], "a session with a live runner must not be re-spawned")
			}
			assert.Len(t, got, tc.wantPubs, "published nudges")
		})
	}
}

// The over-or-not decision is read through the caller's reader, not its cached
// client. Here the two disagree the way a lagging informer does — the cache
// still shows Running for a session that has already failed — and the refusal
// proves which one was consulted.
func TestNotify_ReadsTheSessionThroughTheSuppliedReader(t *testing.T) {
	stale := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "builder"},
		Status:     v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning},
	}
	live := stale.DeepCopy()
	live.Status.Phase = v1.AgentSessionPhaseFailed

	cached := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(stale).
		WithStatusSubresource(&v1.AgentSession{}).Build()
	var reader client.Reader = fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(live).
		WithStatusSubresource(&v1.AgentSession{}).Build()
	mem := memory.NewLocal(inmem.NewBackend())

	err := Notify(context.Background(), cached, reader, mem, nil, time.Now, "ns", "builder", "The test ended.")

	require.ErrorIs(t, err, ErrSessionOver, "the uncached read is the one that decides")
	turns, rerr := turn.ReadAll(memory.WithSystemApproval(context.Background(), "test"), mem, memory.Scope{Kind: "session", ID: "ns/builder"})
	require.NoError(t, rerr)
	assert.Empty(t, turns, "nothing is appended to a session that is over")
}

// The collision this role assignment removes, at the memory layer that
// produced it. A runner parked on await_user_message holds nextIndex = N+1
// inside its blocked dispatch, and the first durable write it makes on resume
// is a tool_result turn at (N+1, "user"). turn.Appender keys an entry on
// (index, role), so a notice that took the "user" role at that same index
// makes the runner's own append fail ErrIndexConflict and takes the whole
// session down. Under the "inbox" role the two keys differ and both land.
func TestAppendNotice_TakesTheInboxRoleSoTheRunnersOwnTurnStillLands(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/builder"}
	app := turn.NewAppender(mem, scope)
	require.NoError(t, app.Append(ctx, memory.Turn{
		Index: 0, Role: "user",
		Content: []memory.ContentBlock{{Type: "text", Text: "build me an agent"}},
	}), "seed the transcript the builder already has")
	require.NoError(t, app.Append(ctx, memory.Turn{
		Index: 1, Role: "assistant",
		Content: []memory.ContentBlock{{Type: "text", Text: "here is the link"}},
	}), "the assistant turn the runner is dispatching from")

	at := time.Date(2026, 9, 13, 4, 0, 0, 0, time.UTC)
	require.NoError(t, AppendNotice(ctx, mem, "ns", "builder", "The person started testing the agent.", at))

	turns, err := turn.ReadAll(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, turns, 3)
	assert.Equal(t, 2, turns[2].Index, "the notice lands at max(index)+1")
	assert.Equal(t, "inbox", turns[2].Role, "an out-of-band writer never takes the runner's own role")
	assert.Equal(t, at, turns[2].CreatedAt, "the turn is stamped with the caller's clock, not time.Now()")

	// The runner resumes and writes the tool_result turn at the index it was
	// holding all along: same number, different role.
	require.NoError(t, app.Append(ctx, memory.Turn{
		Index: 2, Role: "user",
		Content: []memory.ContentBlock{{Type: "tool_result", Text: "the tool answered"}},
	}), "the runner's own turn must still land at the index it held")

	turns, err = turn.ReadAll(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, turns, 4, "both the notice and the runner's turn are in the transcript")
	assert.Equal(t, "inbox", turns[2].Role)
	assert.Equal(t, "user", turns[3].Role)
	assert.Equal(t, 2, turns[3].Index)
}

// failingPutMemory answers every Put with a backend failure, so the notice
// never lands. Reads pass through, so the transcript a test inspects
// afterwards is the real one.
type failingPutMemory struct{ inner memory.Memory }

func (m *failingPutMemory) Put(context.Context, memory.Entry) (memory.Entry, error) {
	return memory.Entry{}, errors.New("the backend is down")
}

func (m *failingPutMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	return m.inner.Query(ctx, q)
}

func (m *failingPutMemory) Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	return m.inner.Search(ctx, req)
}

func (m *failingPutMemory) SendSignal(ctx context.Context, sig memory.Signal) error {
	return m.inner.SendSignal(ctx, sig)
}

// A wake that failed behind a line that landed is its own answer. The caller
// must be able to tell it from a line that never landed, because the two ask
// for opposite things: re-attempting a delivered line appends it twice, while
// re-attempting an undelivered one is the only way it ever arrives.
func TestNotify_WakeOnlyFailureIsTypedAndLeavesTheLineInPlace(t *testing.T) {
	sess := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "builder"},
		Status:     v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseIdle},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sess).
		WithStatusSubresource(&v1.AgentSession{}).Build()
	mem := memory.NewLocal(inmem.NewBackend())
	publish := func(context.Context, string, string, channelevents.Envelope) error {
		return errors.New("the bus is down")
	}

	err := Notify(context.Background(), c, c, mem, publish, time.Now, "ns", "builder", "The test ended.")

	var wakeErr *WakeError
	require.ErrorAs(t, err, &wakeErr, "a wake that failed behind a line that landed is reported as such")
	assert.NotErrorIs(t, err, ErrSessionOver, "the session is live")

	turns, rerr := turn.ReadAll(memory.WithSystemApproval(context.Background(), "test"), mem, memory.Scope{Kind: "session", ID: "ns/builder"})
	require.NoError(t, rerr)
	require.Len(t, turns, 1, "the line landed exactly once and must not be re-attempted")
	assert.Equal(t, "The test ended.", turns[0].Content[0].Text)
}

// A line that never landed is NOT a WakeError, and the wake is not attempted
// on its own: waking an agent to read something that is not there spawns a
// runner for nothing.
func TestNotify_AppendFailureIsNotAWakeErrorAndSkipsTheWake(t *testing.T) {
	sess := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "builder"},
		Status:     v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseIdle},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sess).
		WithStatusSubresource(&v1.AgentSession{}).Build()
	inner := memory.NewLocal(inmem.NewBackend())
	var mem memory.Memory = &failingPutMemory{inner: inner}
	published := 0
	publish := func(context.Context, string, string, channelevents.Envelope) error {
		published++
		return nil
	}

	err := Notify(context.Background(), c, c, mem, publish, time.Now, "ns", "builder", "The test ended.")

	require.Error(t, err, "a line that never landed must reach the caller")
	var wakeErr *WakeError
	assert.False(t, errors.As(err, &wakeErr), "nothing landed, so this is not a wake-only failure")
	assert.Zero(t, published, "there is nothing for the agent to read, so it is not woken")

	turns, rerr := turn.ReadAll(memory.WithSystemApproval(context.Background(), "test"), inner, memory.Scope{Kind: "session", ID: "ns/builder"})
	require.NoError(t, rerr)
	assert.Empty(t, turns)
}

// competingWriter appends one "inbox" turn at the index AppendNotice is about
// to claim, at the moment named by when — before the conflict check reads
// (the collision turn.Appender answers ErrIndexConflict) or after it (the
// collision the facade's append-only guard answers). Both are the same lost
// race with channelsd, which writes a person's own message into the same
// inbox with no coordination with this package.
type competingWriter struct {
	inner memory.Memory
	scope memory.Scope
	when  string // "query" or "put"
	times int    // injections left; each one claims the next index in turn
	next  int
}

func (m *competingWriter) inject(ctx context.Context) {
	if m.times <= 0 {
		return
	}
	m.times--
	_ = turn.NewAppender(m.inner, m.scope).Append(ctx, memory.Turn{
		Index:     m.next,
		Role:      "inbox",
		Content:   []memory.ContentBlock{{Type: "text", Text: "a message the person typed"}},
		CreatedAt: time.Date(2026, 9, 13, 3, 59, 0, 0, time.UTC),
	})
	m.next++
}

func (m *competingWriter) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	// Only the conflict check names an ID; the tail read does not.
	if m.when == "query" && len(q.IDs) > 0 {
		m.inject(ctx)
	}
	return m.inner.Query(ctx, q)
}

func (m *competingWriter) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if m.when == "put" {
		m.inject(ctx)
	}
	return m.inner.Put(ctx, e)
}

func (m *competingWriter) Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	return m.inner.Search(ctx, req)
}

func (m *competingWriter) SendSignal(ctx context.Context, sig memory.Signal) error {
	return m.inner.SendSignal(ctx, sig)
}

// The notice has no index of its own: it claims max+1 at the moment it is
// written, and channelsd claims the same index for a person's own message
// with no coordination. A lost race is recoverable — re-read the tail, take
// the index that is free now — and both lines end up in the transcript.
func TestAppendNotice_RetriesPastALostRaceForTheNextIndex(t *testing.T) {
	seed := func(t *testing.T, when string) (memory.Memory, memory.Memory, memory.Scope) {
		t.Helper()
		ctx := memory.WithSystemApproval(context.Background(), "test")
		inner := memory.NewLocal(inmem.NewBackend())
		scope := memory.Scope{Kind: "session", ID: "ns/builder"}
		app := turn.NewAppender(inner, scope)
		require.NoError(t, app.Append(ctx, memory.Turn{
			Index: 0, Role: "user",
			Content: []memory.ContentBlock{{Type: "text", Text: "build me an agent"}},
		}))
		require.NoError(t, app.Append(ctx, memory.Turn{
			Index: 1, Role: "assistant",
			Content: []memory.ContentBlock{{Type: "text", Text: "here is the link"}},
		}))
		var racey memory.Memory = &competingWriter{inner: inner, scope: scope, when: when, times: 1, next: 2}
		return racey, inner, scope
	}

	for _, when := range []string{"query", "put"} {
		t.Run("a competing inbox turn taken at the "+when+": the notice lands at the next free index", func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			racey, inner, scope := seed(t, when)

			at := time.Date(2026, 9, 13, 4, 0, 0, 0, time.UTC)
			require.NoError(t, AppendNotice(ctx, racey, "ns", "builder", "The person started testing the agent.", at),
				"a lost race for the index is recoverable, not a dropped line")

			turns, err := turn.ReadAll(ctx, inner, scope)
			require.NoError(t, err)
			require.Len(t, turns, 4, "both the person's message and the notice are in the transcript")
			assert.Equal(t, "a message the person typed", turns[2].Content[0].Text)
			assert.Equal(t, 3, turns[3].Index, "the notice took the index that was free on the re-read")
			assert.Equal(t, "inbox", turns[3].Role)
			assert.Equal(t, "The person started testing the agent.", turns[3].Content[0].Text)
		})
	}

	// A writer that takes every index the notice reaches for is reported
	// rather than retried forever.
	t.Run("a race lost on every attempt: reported, and the retry is bounded", func(t *testing.T) {
		ctx := memory.WithSystemApproval(context.Background(), "test")
		inner := memory.NewLocal(inmem.NewBackend())
		scope := memory.Scope{Kind: "session", ID: "ns/builder"}
		var racey memory.Memory = &competingWriter{inner: inner, scope: scope, when: "query", times: 100, next: 0}

		err := AppendNotice(ctx, racey, "ns", "builder", "The test ended.", time.Date(2026, 9, 13, 4, 0, 0, 0, time.UTC))

		require.Error(t, err, "a notice that never found a free index must reach the caller")
		turns, rerr := turn.ReadAll(ctx, inner, scope)
		require.NoError(t, rerr)
		assert.Len(t, turns, 3, "the retry is bounded at three attempts, not unbounded")
		for _, tn := range turns {
			assert.Equal(t, "a message the person typed", tn.Content[0].Text, "the notice never landed")
		}
	})
}
