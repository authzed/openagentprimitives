package memstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/memstream"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolsession"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

const (
	testNS    = "default"
	testName  = "s1"
	testToken = "tok-1"
)

// sessionScope is the scope every test seeds into.
var sessionScope = memory.Scope{Kind: "session", ID: testNS + "/" + testName}

// newMemoryServer stands up the v2 memory HTTP API over an in-memory
// Backend with a per-session token, and returns the server, a
// turn.Appender for seeding the session's transcript, and the backing
// memory.Memory for seeding other Kinds (e.g. tool_session).
func newMemoryServer(t *testing.T) (*httptest.Server, *turn.Appender, memory.Memory) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: testNS, Name: testName}, testToken, "")
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg))
	t.Cleanup(srv.Close)
	app := turn.NewAppender(mem, sessionScope)
	return srv, app, mem
}

// TestStreamerEmitsNewTurns verifies the streamer emits each turn once, in
// (Index, Role) order, picking up a turn appended after streaming starts.
func TestStreamerEmitsNewTurns(t *testing.T) {
	srv, app, _ := newMemoryServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = memory.WithSystemApproval(ctx, "test")

	require.NoError(t, app.Append(ctx, memory.Turn{
		Index: 0, Role: "user",
		Content:   []memory.ContentBlock{{Type: "text", Text: "hello"}},
		CreatedAt: time.Unix(0, 0).UTC(),
	}), "seed turn 0")

	streamer := memstream.New(srv.URL, testNS, testName, testToken, 50*time.Millisecond)
	ch, errCh := streamer.Stream(ctx)

	got := []memory.Turn{}
	for tn := range ch {
		got = append(got, tn)
		switch len(got) {
		case 1:
			// Append the second turn only after the first has streamed,
			// so we exercise the seen-count diffing on the next poll.
			require.NoError(t, app.Append(ctx, memory.Turn{
				Index: 1, Role: "assistant",
				Content:   []memory.ContentBlock{{Type: "text", Text: "hi"}},
				CreatedAt: time.Unix(1, 0).UTC(),
			}), "append turn 1")
		case 2:
			cancel()
		}
	}
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("stream err: %v", err)
	}
	require.Len(t, got, 2)
	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, "hello", got[0].Content[0].Text)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Equal(t, "hi", got[1].Content[0].Text)
}

// captureSlog swaps the default slog logger for one writing into the returned
// buffer, restoring the previous logger at test end. The skip path reports
// through slog.Default() — cmd/oap installs no handler, so the default one puts
// these lines on the user's stderr — and this is how a test observes that a
// dropped record was announced rather than swallowed.
//
// It mutates package-global state, so no test using it may be parallel.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// seedPoisonedTranscript writes two well-formed turns around one entry that no
// reader of the turn Kind can decode, and returns the backing memory.
//
// The poison is written through the same Put a session's own bearer reaches over
// HTTP: turn is BOTH append-only and session-written, and Local.Put validates the
// Kind, the write authority and the ID PREFIX — never the shape. EntryToTurn
// Sscanfs the ID, so "turn-x" with EMPTY content suffices. Nothing can remove it
// afterwards: per-entry Delete is refused unconditionally for an append-only Kind
// and DeleteScope skips it. It sits BETWEEN the two good turns so a reader that
// stops on it is distinguishable from one that skips it.
func seedPoisonedTranscript(t *testing.T, app *turn.Appender, mem memory.Memory) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, app.Append(ctx, memory.Turn{
		Index: 0, Role: "user",
		Content:   []memory.ContentBlock{{Type: "text", Text: "before"}},
		CreatedAt: time.Unix(0, 0).UTC(),
	}), "seed the turn before the poison")

	_, err := mem.Put(ctx, memory.Entry{
		Scope:      sessionScope,
		Kind:       turn.KindName,
		ID:         "turn-x",
		CreatedAt:  time.Unix(1, 0).UTC(),
		Provenance: &memory.Provenance{Publisher: "session:" + testNS + "/" + testName, KeyID: "k1"},
	})
	require.NoError(t, err, "the poisoned entry is accepted — Put validates the ID prefix, never the shape")

	require.NoError(t, app.Append(ctx, memory.Turn{
		Index: 1, Role: "assistant",
		Content:   []memory.ContentBlock{{Type: "text", Text: "after"}},
		CreatedAt: time.Unix(2, 0).UTC(),
	}), "seed the turn after the poison")
}

// TestStream_PoisonedTurnEntryIsSkippedNotFatal pins that one undecodable entry
// degrades the streamed transcript instead of ending it.
//
// A hard error here does not merely abandon one command. Stream's converter sent
// the error and RETURNED, closing the channel: oap agent run (agent_run.go) sees
// turnCh closed and jumps to its terminal block, so a live run goes DARK for the
// rest of the session while the agent keeps working server-side. Re-running never
// helps — the entry is undeletable, so every future invocation re-poisons at the
// same point. That is the same permanent capability loss the four in-process
// readers had, minus the requeue loop.
//
// The transcript is a RECORD, not an input to a gate, so skip is the correct
// disposition — the same one turn.ReadAll takes over the same rows.
func TestStream_PoisonedTurnEntryIsSkippedNotFatal(t *testing.T) {
	logs := captureSlog(t)
	srv, app, mem := newMemoryServer(t)
	seedPoisonedTranscript(t, app, mem)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	streamer := memstream.New(srv.URL, testNS, testName, testToken, 25*time.Millisecond)
	ch, errCh := streamer.Stream(ctx)

	got := []memory.Turn{}
	for tn := range ch {
		got = append(got, tn)
		if len(got) == 2 {
			cancel()
		}
	}
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("stream err: %v", err)
	}

	require.Len(t, got, 2, "both well-formed turns must stream past the poisoned entry")
	assert.Equal(t, "before", got[0].Content[0].Text)
	assert.Equal(t, "after", got[1].Content[0].Text, "the turn AFTER the poison is what a halting reader loses")

	// The drop must be findable: which record vanished, and who signed it
	// (AGENTS.md — never silently drop an error).
	assert.Contains(t, logs.String(), "turn-x", "the skipped entry's ID must reach the log")
	assert.Contains(t, logs.String(), "session:"+testNS+"/"+testName,
		"the publisher that signed the unreadable entry")
}

// TestFetch_PoisonedTurnEntryIsSkippedNotFatal pins the same disposition for the
// one-shot read. Fetch returning an error makes the session's transcript
// permanently unreadable by oap session logs / oap session operations, with no
// operator remedy, since the entry cannot be deleted.
func TestFetch_PoisonedTurnEntryIsSkippedNotFatal(t *testing.T) {
	logs := captureSlog(t)
	srv, app, mem := newMemoryServer(t)
	seedPoisonedTranscript(t, app, mem)

	streamer := memstream.New(srv.URL, testNS, testName, testToken, time.Second)
	got, err := streamer.Fetch(t.Context())
	require.NoError(t, err, "one unreadable entry must not fail the whole transcript read")

	require.Len(t, got, 2, "both well-formed turns survive the poisoned one")
	assert.Equal(t, 0, got[0].Index, "sorted ascending by Index")
	assert.Equal(t, "before", got[0].Content[0].Text)
	assert.Equal(t, "after", got[1].Content[0].Text)

	assert.Contains(t, logs.String(), "turn-x", "the skipped entry's ID must reach the log")
	assert.Contains(t, logs.String(), "session:"+testNS+"/"+testName,
		"the publisher that signed the unreadable entry")
}

// TestStreamerUnauthorized verifies that a wrong bearer token surfaces as
// an unrecoverable error on errCh rather than silent emptiness.
func TestStreamerUnauthorized(t *testing.T) {
	srv, _, _ := newMemoryServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	streamer := memstream.New(srv.URL, testNS, testName, "wrong-token", 10*time.Millisecond)
	ch, errCh := streamer.Stream(ctx)
	for range ch { //nolint:revive // drain until the streamer gives up
	}
	err := <-errCh
	require.Error(t, err, "expected an error after repeated 403s")
	assert.Contains(t, err.Error(), "memstream:", "error should be tagged")
}

// TestFetchOneShot verifies Fetch returns the full transcript in
// (Index, Role) order without streaming.
func TestFetchOneShot(t *testing.T) {
	srv, app, _ := newMemoryServer(t)
	ctx := memory.WithSystemApproval(t.Context(), "test")

	require.NoError(t, app.Append(ctx, memory.Turn{
		Index: 1, Role: "assistant",
		Content: []memory.ContentBlock{{Type: "text", Text: "second"}},
	}), "append turn 1")
	require.NoError(t, app.Append(ctx, memory.Turn{
		Index: 0, Role: "user",
		Content: []memory.ContentBlock{{Type: "text", Text: "first"}},
	}), "append turn 0")

	streamer := memstream.New(srv.URL, testNS, testName, testToken, time.Second)
	got, err := streamer.Fetch(ctx)
	require.NoError(t, err, "Fetch")
	require.Len(t, got, 2)
	assert.Equal(t, 0, got[0].Index, "sorted ascending by Index")
	assert.Equal(t, "first", got[0].Content[0].Text)
	assert.Equal(t, 1, got[1].Index)
	assert.Equal(t, "second", got[1].Content[0].Text)
}

// TestStreamEntriesEmitsNewEntries verifies StreamEntries streams raw
// memory.Entry of an arbitrary Kind (tool_session here): three seeded
// events arrive in CreatedAt order, and a fourth recorded mid-stream is
// emitted alone on the next poll (the seen-count diff).
func TestStreamEntriesEmitsNewEntries(t *testing.T) {
	srv, _, mem := newMemoryServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Three events with monotonically increasing CreatedAt; toolsession.Record
	// stamps time.Now(), so a brief gap between calls is enough to order them.
	for i := 0; i < 3; i++ {
		require.NoError(t, toolsession.Record(ctx, mem, sessionScope, toolsession.Event{
			ToolCallRef: "tc-1",
			EventType:   "text_delta",
			Text:        []string{"a", "b", "c"}[i],
		}), "seed event")
		time.Sleep(time.Millisecond)
	}

	streamer := memstream.New(srv.URL, testNS, testName, testToken, 50*time.Millisecond)
	ch, errCh := streamer.StreamEntries(ctx, toolsession.KindName)

	got := []memory.Entry{}
	for e := range ch {
		got = append(got, e)
		switch len(got) {
		case 3:
			// All three seeded events streamed; append a fourth so the next
			// poll exercises the seen-count diff.
			require.NoError(t, toolsession.Record(ctx, mem, sessionScope, toolsession.Event{
				ToolCallRef: "tc-1",
				EventType:   "result",
				OK:          true,
			}), "record fourth event")
		case 4:
			cancel()
		}
	}
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("stream err: %v", err)
	}
	require.Len(t, got, 4, "three seeded + one mid-stream event")

	// Entries arrive in ascending CreatedAt order.
	for i := 1; i < len(got); i++ {
		assert.Falsef(t, got[i].CreatedAt.Before(got[i-1].CreatedAt),
			"entry %d (%s) must not precede entry %d (%s)",
			i, got[i].CreatedAt, i-1, got[i-1].CreatedAt)
	}

	first, err := toolsession.EntryToEvent(got[0])
	require.NoError(t, err, "decode first event")
	assert.Equal(t, "a", first.Text)
	last, err := toolsession.EntryToEvent(got[3])
	require.NoError(t, err, "decode fourth event")
	assert.Equal(t, "result", last.EventType, "fourth event emitted alone after seen-count diff")
}

// TestStreamEntriesEqualCreatedAtNotDropped verifies that entries sharing
// an identical CreatedAt (a burst of tool_session events parsed from one
// stdout chunk) are all emitted exactly once. fetchEntries' sort breaks
// CreatedAt ties on Entry.ID for a deterministic total order; without that
// tiebreaker the non-stable sort over map-randomized Query output could
// reorder equal-timestamp entries between polls and make streamEntries'
// seen-count diff silently drop one.
func TestStreamEntriesEqualCreatedAtNotDropped(t *testing.T) {
	srv, _, mem := newMemoryServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Seed several events that all share one CreatedAt instant. Record
	// stamps time.Now(), so write the entries directly with a fixed
	// timestamp and unique IDs to reproduce the burst condition exactly.
	const burst = 5
	shared := time.Unix(1700000000, 0).UTC()
	for i := 0; i < burst; i++ {
		raw, merr := json.Marshal(toolsession.Event{
			ToolCallRef: "tc-1",
			EventType:   "text_delta",
			Text:        fmt.Sprintf("chunk-%d", i),
		})
		require.NoError(t, merr, "marshal event")
		_, perr := mem.Put(ctx, memory.Entry{
			Scope:     sessionScope,
			Kind:      toolsession.KindName,
			ID:        memory.NewID(toolsession.Kind{}),
			CreatedAt: shared,
			Content:   raw,
		})
		require.NoError(t, perr, "put burst event")
	}

	streamer := memstream.New(srv.URL, testNS, testName, testToken, 25*time.Millisecond)
	ch, errCh := streamer.StreamEntries(ctx, toolsession.KindName)

	got := []memory.Entry{}
	for e := range ch {
		got = append(got, e)
		if len(got) == burst {
			cancel()
		}
	}
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("stream err: %v", err)
	}
	require.Len(t, got, burst, "every equal-CreatedAt entry must be emitted, none dropped")

	// IDs must be unique across what was emitted — no entry re-emitted.
	seen := map[string]bool{}
	for _, e := range got {
		assert.Falsef(t, seen[e.ID], "entry ID %s emitted twice", e.ID)
		seen[e.ID] = true
		assert.True(t, e.CreatedAt.Equal(shared), "all emitted entries share the seeded CreatedAt")
	}
}
