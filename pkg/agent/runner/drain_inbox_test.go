package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// drainTextTurn builds a one-text-block memory.Turn at (idx, role).
func drainTextTurn(idx int, role, text string) memory.Turn {
	return memory.Turn{
		Index:   idx,
		Role:    role,
		Content: []memory.ContentBlock{{Type: "text", Text: text}},
	}
}

// newDrainLoop builds a minimal *Loop with only Memory wired — every
// binding dependency (EntityExtractor, AgentClass, AgentSession, Status)
// is left nil so processBindingsForUserTurn early-returns and drainInbox
// can be exercised in isolation.
func newDrainLoop(t *testing.T) *Loop {
	t.Helper()
	store := LocalMemoryAdapter(
		memory.NewLocal(inmem.NewBackend()),
		memory.NamespacedName{Namespace: "default", Name: "s1"},
	)
	return &Loop{Memory: store}
}

// firstText returns the text of a turn's first content block.
func firstText(t *testing.T, tn memory.Turn) string {
	t.Helper()
	require.NotEmpty(t, tn.Content, "turn has no content blocks")
	return tn.Content[0].Text
}

// TestDrainInbox_PlacesInboxAsUserTurnsAndMarkers proves drainInbox
// converts "inbox"-role turns into runner-indexed "user" turns, writes a
// paired "inbox_done" marker per consumed entry, and advances nextIndex.
func TestDrainInbox_PlacesInboxAsUserTurnsAndMarkers(t *testing.T) {
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Prior transcript: user(0), assistant(1). channelsd then wrote an
	// inbound human reply as an "inbox"-role turn at max(Index)+1 == 2.
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "do the thing")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "working")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(2, "inbox", "also open a 5th PR")))

	placed, newNext, err := l.drainInbox(ctx, 2)
	require.NoError(t, err, "drainInbox must succeed")

	require.Len(t, placed, 1, "one inbox turn must be placed as a user turn")
	assert.Equal(t, "user", placed[0].Role, "placed turn is a real user turn")
	assert.Equal(t, 2, placed[0].Index, "placed user turn lands at the runner-assigned index")
	assert.Equal(t, "also open a 5th PR", firstText(t, placed[0]), "content carried over from inbox turn")
	assert.Equal(t, 3, newNext, "nextIndex advanced past the placed turn")

	all, err := l.Memory.ReadAll(ctx)
	require.NoError(t, err, "ReadAll after drain")

	byRole := map[string]int{}
	for _, tn := range all {
		byRole[tn.Role]++
	}
	assert.Equal(t, 2, byRole["user"], "original user(0) + drained user(2)")
	assert.Equal(t, 1, byRole["assistant"], "assistant(1) untouched")
	assert.Equal(t, 1, byRole["inbox"], "the inbox turn itself stays in memory")
	assert.Equal(t, 1, byRole["inbox_done"], "a consumed marker was written for the inbox turn")

	// The marker shares the inbox turn's Index so a later drain skips it.
	var markerIdx = -1
	for _, tn := range all {
		if tn.Role == "inbox_done" {
			markerIdx = tn.Index
		}
	}
	assert.Equal(t, 2, markerIdx, "inbox_done marker keyed on the inbox turn's Index")
}

// TestDrainInbox_SecondDrainIsIdempotent proves a consumed "inbox" turn
// (one with a paired "inbox_done" marker) is not re-placed on a later
// drain — markers are honored.
func TestDrainInbox_SecondDrainIsIdempotent(t *testing.T) {
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "start")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "ok")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(2, "inbox", "follow-up")))

	placed, newNext, err := l.drainInbox(ctx, 2)
	require.NoError(t, err, "first drain")
	require.Len(t, placed, 1, "first drain places the inbox turn")

	placed2, newNext2, err := l.drainInbox(ctx, newNext)
	require.NoError(t, err, "second drain")
	assert.Empty(t, placed2, "second drain places nothing — the marker is honored")
	assert.Equal(t, newNext, newNext2, "nextIndex unchanged when nothing is drained")
}

// TestDrainInbox_NoInboxTurnsIsNoOp proves drainInbox is a no-op (no
// placed turns, nextIndex unchanged) when there are no "inbox" turns.
func TestDrainInbox_NoInboxTurnsIsNoOp(t *testing.T) {
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "hello")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "hi")))

	placed, newNext, err := l.drainInbox(ctx, 2)
	require.NoError(t, err, "drainInbox on a session with no inbox turns")
	assert.Empty(t, placed, "no inbox turns means nothing is placed")
	assert.Equal(t, 2, newNext, "nextIndex unchanged")
}

// TestDrainInbox_MultipleInboxTurnsPlacedAscending proves several
// pending "inbox" turns are drained in ascending Index order into
// contiguous runner-assigned "user" indices.
func TestDrainInbox_MultipleInboxTurnsPlacedAscending(t *testing.T) {
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "start")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "ok")))
	// Two queued inbound messages, written by channelsd at successive indices.
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(2, "inbox", "first reply")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(3, "inbox", "second reply")))

	placed, newNext, err := l.drainInbox(ctx, 2)
	require.NoError(t, err, "drainInbox with multiple inbox turns")

	require.Len(t, placed, 2, "both inbox turns placed")
	assert.Equal(t, "first reply", firstText(t, placed[0]), "ascending order: first inbox turn first")
	assert.Equal(t, "second reply", firstText(t, placed[1]), "ascending order: second inbox turn second")
	assert.Equal(t, 2, placed[0].Index, "first placed user turn at runner index 2")
	assert.Equal(t, 3, placed[1].Index, "second placed user turn at runner index 3")
	assert.Equal(t, 4, newNext, "nextIndex advanced past both placed turns")
}

// TestDrainInbox_PreservesAuthor proves the "user" turn drainInbox
// re-appends carries the source "inbox" turn's Author (channelsd stamps
// this on inbound human messages), rather than dropping it. Runner-
// authored artifacts (the inbox_done marker here) stay authorless.
func TestDrainInbox_PreservesAuthor(t *testing.T) {
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "start")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "ok")))
	inboxTurn := drainTextTurn(3, "inbox", "second user says hi")
	inboxTurn.Author = identity.Subject("user:Ym9i")
	require.NoError(t, l.Memory.Append(ctx, inboxTurn))

	placed, _, err := l.drainInbox(ctx, 10)
	require.NoError(t, err, "drainInbox must succeed")

	require.Len(t, placed, 1, "one inbox turn must be placed as a user turn")
	assert.Equal(t, "user", placed[0].Role, "placed turn is a real user turn")
	assert.Equal(t, identity.Subject("user:Ym9i"), placed[0].Author,
		"the drained user turn must carry the inbox turn's author, not empty")

	all, err := l.Memory.ReadAll(ctx)
	require.NoError(t, err, "ReadAll after drain")
	for _, tn := range all {
		if tn.Role == "inbox_done" {
			assert.Empty(t, tn.Author, "the inbox_done marker is runner-authored and carries no human author")
		}
	}
}

// TestDrainInbox_PreservesVia proves the "user" turn drainInbox re-appends
// carries the source "inbox" turn's Via (a channelkinds surface stamps this
// on inbound view-originated messages — see channelkinds/InboundEvent),
// rather than dropping it. Without this, replay (loop_replay.go) never sees the Via
// on the promoted turn and can't render the "(via ...)" annotation for a
// follow-up message that arrived through drainInbox instead of turn 0.
func TestDrainInbox_PreservesVia(t *testing.T) {
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "start")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "ok")))
	inboxTurn := drainTextTurn(2, "inbox", "the CTA padding is wrong")
	inboxTurn.Via = "urn:ap:view:artifact:artifact-3f2a1b8c"
	require.NoError(t, l.Memory.Append(ctx, inboxTurn))

	placed, _, err := l.drainInbox(ctx, 2)
	require.NoError(t, err, "drainInbox must succeed")

	require.Len(t, placed, 1, "one inbox turn must be placed as a user turn")
	assert.Equal(t, "user", placed[0].Role, "placed turn is a real user turn")
	assert.Equal(t, "urn:ap:view:artifact:artifact-3f2a1b8c", placed[0].Via,
		"the drained user turn must carry the inbox turn's Via, not empty")
}

// fastAwaitResumeRetry shrinks drainAwaitResume's bounded retry to run instantly
// under test and restores the production values on cleanup. Tests using it must
// NOT run in parallel (it mutates package globals).
func fastAwaitResumeRetry(t *testing.T) {
	t.Helper()
	origRetries, origBackoff := awaitResumeDrainRetries, awaitResumeDrainRetryBackoff
	awaitResumeDrainRetries = 5
	awaitResumeDrainRetryBackoff = time.Millisecond
	t.Cleanup(func() {
		awaitResumeDrainRetries, awaitResumeDrainRetryBackoff = origRetries, origBackoff
	})
}

// firstReadsEmptyMemory wraps a MemoryAppender and hides ALL turns from the
// first hideFirst ReadAll calls, simulating the read-after-write gap where
// channelsd's just-written "inbox" turn is not yet visible to the runner's
// drain read. Append / ReadAfter pass straight through.
type firstReadsEmptyMemory struct {
	inner     MemoryAppender
	hideFirst int
	reads     int
}

func (m *firstReadsEmptyMemory) ReadAll(ctx context.Context) ([]memory.Turn, error) {
	m.reads++
	if m.reads <= m.hideFirst {
		return nil, nil
	}
	return m.inner.ReadAll(ctx)
}

func (m *firstReadsEmptyMemory) ReadAfter(ctx context.Context, after int) ([]memory.Turn, error) {
	return m.inner.ReadAfter(ctx, after)
}

func (m *firstReadsEmptyMemory) Append(ctx context.Context, t memory.Turn) error {
	return m.inner.Append(ctx, t)
}

// TestDrainAwaitResume_RetriesUntilVisibleThenDrains proves the await-resume
// drain recovers a just-arrived reply that isn't visible on the first read —
// the read-after-write race that stranded turn-000005-inbox and blew a session's
// token budget. The first drain read is hidden; the bounded retry re-reads,
// finds the inbox turn, promotes it, and writes its inbox_done marker.
func TestDrainAwaitResume_RetriesUntilVisibleThenDrains(t *testing.T) {
	fastAwaitResumeRetry(t)
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "brief")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "clarifying questions")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(2, "inbox", "1. yes 2. broadly")))

	racey := &firstReadsEmptyMemory{inner: l.Memory, hideFirst: 1}
	l.Memory = racey

	placed, next, err := l.drainAwaitResume(ctx, 3)
	require.NoError(t, err, "drainAwaitResume must succeed")
	require.Len(t, placed, 1, "the retry must recover the initially-hidden inbox turn")
	assert.Equal(t, "user", placed[0].Role, "recovered turn is promoted to a real user turn")
	assert.Equal(t, "1. yes 2. broadly", firstText(t, placed[0]), "the reply's content is delivered")
	assert.Equal(t, 4, next, "nextIndex advanced past the drained turn")
	assert.GreaterOrEqual(t, racey.reads, 2, "must have re-read (first read was hidden)")

	all, err := l.Memory.ReadAll(ctx)
	require.NoError(t, err, "ReadAll after drain")
	markers := 0
	for _, tn := range all {
		if tn.Role == "inbox_done" {
			markers++
		}
	}
	assert.Equal(t, 1, markers, "the recovered inbox turn is now marked consumed (not stranded)")
}

// TestDrainAwaitResume_StillEmptyAfterRetriesReturnsEmptyNoError proves a resume
// with genuinely nothing to drain (a spurious/duplicate wake, or a message that
// never becomes visible) is bounded and non-fatal: it returns empty without
// error or hang, leaving nextIndex untouched. The loud log (not asserted here)
// keeps it from being silent.
func TestDrainAwaitResume_StillEmptyAfterRetriesReturnsEmptyNoError(t *testing.T) {
	fastAwaitResumeRetry(t)
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "brief")))

	placed, next, err := l.drainAwaitResume(ctx, 1)
	require.NoError(t, err, "a genuinely empty held-inbox is recoverable, not an error")
	assert.Empty(t, placed, "nothing to drain → nothing placed")
	assert.Equal(t, 1, next, "nextIndex unchanged when nothing drained")
}

// TestDrainInbox_OperatorNoticeIsSkippedByReplayThenPromoted proves the whole
// contract an out-of-band writer relies on when it appends to a live session:
// pkg/controllers/inboxwake writes a fixed notice as an "inbox" turn with no
// Author and no Via (nobody sent it, and it came through no view), and the
// runner does the rest. replay skips it — it is not a "user" or "assistant"
// turn — while still advancing nextIndex past it, so the loop's own next
// append cannot land on the notice's index; then drainInbox promotes it into a
// real "user" turn at an index the runner assigned itself, which is what puts
// the notice in front of the model.
func TestDrainInbox_OperatorNoticeIsSkippedByReplayThenPromoted(t *testing.T) {
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	const notice = "The person started testing the agent."
	prior := []memory.Turn{
		drainTextTurn(0, "user", "build me an agent"),
		drainTextTurn(1, "assistant", "here is the link"),
		drainTextTurn(2, "inbox", notice), // Author and Via deliberately empty
	}
	for _, tn := range prior {
		require.NoError(t, l.Memory.Append(ctx, tn))
	}

	msgs, nextIndex, hadInitialPrompt := replay(prior, "default/s1")

	require.Len(t, msgs, 2, "the notice is not replayed as a message of its own")
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, 3, nextIndex, "nextIndex still advanced past the notice, so no runner turn lands on it")
	assert.True(t, hadInitialPrompt)

	placed, newNext, err := l.drainInbox(ctx, nextIndex)
	require.NoError(t, err, "drainInbox must succeed")

	require.Len(t, placed, 1, "the notice is promoted")
	assert.Equal(t, "user", placed[0].Role, "the model reads it as an inbound message")
	assert.Equal(t, notice, firstText(t, placed[0]))
	assert.Equal(t, 3, placed[0].Index, "at the runner-assigned index, not the notice's own")
	assert.Empty(t, placed[0].Author, "a line nobody sent is attributed to nobody")
	assert.Empty(t, placed[0].Via)
	assert.Equal(t, 4, newNext)
}

// personTurn builds an "inbox" turn a person sent — channelsd stamps an
// Author on every inbound human message, which is the only thing separating
// one from an operator notice written straight into the same inbox.
func personTurn(idx int, text string) memory.Turn {
	tn := drainTextTurn(idx, "inbox", text)
	tn.Author = identity.Subject("user:Ym9i")
	return tn
}

// TestDrainHeldAtYield_AnnouncesOnlyWhatAPersonSent proves the pickup
// announcement counts inbound MESSAGES, not drained turns. The same inbox
// carries operator notices (pkg/controllers/inboxwake writes them with no
// Author, because nobody sent them), and telling a person "Picking up 1
// message you sent." when they sent nothing describes something that did not
// happen — and miscounts when they did send one alongside a notice.
func TestDrainHeldAtYield_AnnouncesOnlyWhatAPersonSent(t *testing.T) {
	const notice = "The person started testing the agent."
	cases := []struct {
		name  string
		inbox []memory.Turn
		want  []string
	}{
		{
			name:  "an operator notice alone: nothing is announced, because the person sent nothing",
			inbox: []memory.Turn{drainTextTurn(2, "inbox", notice)},
			want:  nil,
		},
		{
			name:  "a person's message alongside a notice: only the message is counted",
			inbox: []memory.Turn{drainTextTurn(2, "inbox", notice), personTurn(3, "also try the other prompt")},
			want:  []string{"Picking up 1 message you sent."},
		},
		{
			name:  "a person's message alone: counted, singular",
			inbox: []memory.Turn{personTurn(2, "also try the other prompt")},
			want:  []string{"Picking up 1 message you sent."},
		},
		{
			name:  "two of the person's messages: both counted, plural",
			inbox: []memory.Turn{personTurn(2, "wait"), personTurn(3, "and rename it")},
			want:  []string{"Picking up 2 messages you sent."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newDrainLoop(t)
			ctx := memory.WithSystemApproval(context.Background(), "test")
			require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "build me an agent")))
			require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "here is the link")))
			for _, tn := range tc.inbox {
				require.NoError(t, l.Memory.Append(ctx, tn))
			}
			var said []string
			l.Notify = func(_ context.Context, text string) { said = append(said, text) }

			placed, _, err := l.drainHeldAtYield(ctx, 2)
			require.NoError(t, err, "drainHeldAtYield must succeed")

			require.Len(t, placed, len(tc.inbox), "every queued turn is drained, announced or not")
			assert.Equal(t, tc.want, said)
		})
	}
}
