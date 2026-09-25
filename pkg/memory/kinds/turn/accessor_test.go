package turn_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/memtest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// queryErrMemory is a memory.Memory whose Query always fails. It records
// whether Put was reached so a test can prove Append aborts before the
// blind Put on a failed conflict check.
type queryErrMemory struct {
	queryErr error
	putCalls int
}

func (m *queryErrMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, m.queryErr
}

func (m *queryErrMemory) Put(_ context.Context, e memory.Entry) (memory.Entry, error) {
	m.putCalls++
	return e, nil
}

func (m *queryErrMemory) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}

func (m *queryErrMemory) SendSignal(context.Context, memory.Signal) error { return nil }

// newAppender builds an Appender over a fresh in-memory backend for a
// fixed session scope.
func newAppender(t *testing.T) *turn.Appender {
	t.Helper()
	return turn.NewAppender(
		memory.NewLocal(inmem.NewBackend()),
		memory.Scope{Kind: "session", ID: "default/s1"},
	)
}

// textTurn builds a one-text-block Turn with a deterministic CreatedAt so
// idempotency comparisons are stable.
func textTurn(idx int, role, text string) memory.Turn {
	return memory.Turn{
		Index:     idx,
		Role:      role,
		Content:   []memory.ContentBlock{{Type: "text", Text: text}},
		CreatedAt: time.Unix(int64(idx), 0).UTC(),
	}
}

func TestAppender_RoundTripOrdered(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	user := textTurn(0, "user", "hello")
	assistant := memory.Turn{
		Index:     1,
		Role:      "assistant",
		Content:   []memory.ContentBlock{{Type: "text", Text: "hi there"}},
		CreatedAt: time.Unix(1, 0).UTC(),
		Usage:     &memory.Usage{InputTokens: 12, OutputTokens: 34, CacheReadTokens: 5},
	}
	note := textTurn(2, "system_note", "operator note")

	// Append out of order to prove ReadAll sorts, not insertion-order.
	require.NoError(t, a.Append(ctx, note))
	require.NoError(t, a.Append(ctx, assistant))
	require.NoError(t, a.Append(ctx, user))

	got, err := a.ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 3)

	assert.Equal(t, user.Index, got[0].Index)
	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, user.Content, got[0].Content)
	assert.Nil(t, got[0].Usage)

	assert.Equal(t, assistant.Index, got[1].Index)
	assert.Equal(t, "assistant", got[1].Role)
	assert.Equal(t, assistant.Content, got[1].Content)
	require.NotNil(t, got[1].Usage, "assistant turn must round-trip Usage")
	assert.Equal(t, *assistant.Usage, *got[1].Usage)

	assert.Equal(t, note.Index, got[2].Index)
	assert.Equal(t, "system_note", got[2].Role)
	assert.Equal(t, note.Content, got[2].Content)
}

func TestAppender_IdempotentReappend(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	tn := textTurn(0, "user", "hello")
	require.NoError(t, a.Append(ctx, tn))
	require.NoError(t, a.Append(ctx, tn), "identical re-append must be a no-op")

	got, err := a.ReadAll(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 1, "dedup failed")
}

// TestAppender_IdempotentReappendThroughDurableBackend is
// TestAppender_IdempotentReappend against a backend that reshapes entries on
// the way out, as postgres does (JSONB content, microsecond created_at).
//
// This is the more reachable half of the raw-byte comparison defect: Append's
// conflict check runs on every turn write, so a legitimate re-append — the
// runner replaying a turn after a restart — hits it, not just a retry after a
// lost response. The sibling test above cannot see it because inmem stores
// content verbatim.
func TestAppender_IdempotentReappendThroughDurableBackend(t *testing.T) {
	a := turn.NewAppender(
		memory.NewLocal(memtest.NewRoundTrip(inmem.NewBackend())),
		memory.Scope{Kind: "session", ID: "default/s1"},
	)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	tn := textTurn(0, "user", "hello")
	require.NoError(t, a.Append(ctx, tn))
	require.NoError(t, a.Append(ctx, tn), "identical re-append must be a no-op")

	got, err := a.ReadAll(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 1, "dedup failed")

	// The conflict door must still shut on a genuinely different payload.
	err = a.Append(ctx, textTurn(0, "user", "different"))
	assert.ErrorIs(t, err, memory.ErrIndexConflict,
		"same (index, role) with a different payload must still conflict")
}

func TestAppender_IndexConflict(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, a.Append(ctx, textTurn(0, "user", "hello")))
	err := a.Append(ctx, textTurn(0, "user", "different"))
	assert.ErrorIs(t, err, memory.ErrIndexConflict,
		"same (index, role) with a different payload must conflict")
}

func TestAppender_ReadAfter(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, a.Append(ctx, textTurn(0, "user", "zero")))
	require.NoError(t, a.Append(ctx, textTurn(1, "assistant", "one")))
	require.NoError(t, a.Append(ctx, textTurn(2, "user", "two")))

	got, err := a.ReadAfter(ctx, 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, 1, got[0].Index)
	assert.Equal(t, 2, got[1].Index)
}

func TestAppender_QueryErrorAbortsBeforePut(t *testing.T) {
	sentinel := errors.New("backend down")
	fake := &queryErrMemory{queryErr: sentinel}
	a := turn.NewAppender(fake, memory.Scope{Kind: "session", ID: "default/s1"})

	err := a.Append(context.Background(), textTurn(0, "user", "hello"))
	require.Error(t, err, "a failed conflict-check Query must fail Append")
	assert.ErrorIs(t, err, sentinel, "Append must wrap the underlying Query error")
	assert.Zero(t, fake.putCalls, "Append must not Put after a failed conflict check")
}

func TestEntryToTurn_MalformedID(t *testing.T) {
	_, err := turn.EntryToTurn(memory.Entry{ID: "not-a-turn-id", Kind: "turn"})
	require.Error(t, err, "a non-turn-shaped ID must not decode")
}

// Equivalence port: the scenarios below re-assert pkg/memory/inmem's
// transcript test cases (TestAppendThenReadAll, TestAppendIsIdempotent,
// TestAppendIndexConflict) against the turn.Appender, proving the v2
// migration preserves the legacy inmem.Store behavior.
func TestAppender_PortedInmemBehavior(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("ported: inmem ordering", func(t *testing.T) {
		a := newAppender(t)
		require.NoError(t, a.Append(ctx, textTurn(0, "user", "hello")))
		require.NoError(t, a.Append(ctx, textTurn(1, "assistant", "hi")))
		out, err := a.ReadAll(ctx)
		require.NoError(t, err)
		require.Len(t, out, 2)
		assert.Equal(t, "user", out[0].Role)
		assert.Equal(t, "assistant", out[1].Role)
	})

	t.Run("ported: inmem idempotent re-append", func(t *testing.T) {
		a := newAppender(t)
		require.NoError(t, a.Append(ctx, textTurn(0, "user", "hello")))
		require.NoError(t, a.Append(ctx, textTurn(0, "user", "hello")),
			"duplicate append should be a no-op")
		out, err := a.ReadAll(ctx)
		require.NoError(t, err)
		assert.Len(t, out, 1, "dedup failed")
	})

	t.Run("ported: inmem index conflict", func(t *testing.T) {
		a := newAppender(t)
		require.NoError(t, a.Append(ctx, textTurn(0, "user", "hello")))
		err := a.Append(ctx, textTurn(0, "user", "DIFFERENT"))
		assert.ErrorIs(t, err, memory.ErrIndexConflict)
	})
}

// TestAppender_TwoWriterRace_IndexConflict reproduces the production
// "memory: index conflict" → MemoryUnavailable failure.
//
// channelsd (internal/cmd/channelsd/memory.go) and the runner loop
// (pkg/agent/runner/loop.go) are BOTH writers of user-role turns for a
// single session scope — channelsd appends the human reply, the runner
// appends tool_result turns — and each allocates its index
// independently as max(existing Index)+1. turn.Append's doc says
// correctness "relies on a single writer per session scope"; the
// channelsd comment claims "Channelsd is the only writer of new
// user-role turns ... so there's no concurrent-writer race here" —
// which is false, because the runner writes user-role tool_result
// turns every loop iteration.
//
// When a human reply and a tool_result turn race to the same max+1
// index, the second Append collides on the (index, role) key and
// returns memory.ErrIndexConflict. The runner surfaces that at
// loop.go's "append user turn: %v" as AgentSession reason
// MemoryUnavailable.
func TestAppender_TwoWriterRace_IndexConflict(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// nextIndex mirrors how BOTH writers pick a free index today:
	// internal/cmd/channelsd/memory.go's nextIndex() and the runner loop's
	// nextIndex counter both resolve to max(existing Index)+1.
	nextIndex := func() int {
		all, err := a.ReadAll(ctx)
		require.NoError(t, err)
		max := -1
		for _, tn := range all {
			if tn.Index > max {
				max = tn.Index
			}
		}
		return max + 1
	}

	// Prior conversation: user(0), assistant(1). The assistant turn
	// issued a tool_use, so the runner still owes a tool_result turn.
	require.NoError(t, a.Append(ctx, textTurn(0, "user", "do the thing")))
	require.NoError(t, a.Append(ctx, textTurn(1, "assistant", "calling a tool")))

	// channelsd and the runner each independently compute the next free
	// index from the same memory snapshot — both get 2.
	channelsdIdx := nextIndex()
	runnerIdx := nextIndex()
	require.Equal(t, 2, channelsdIdx, "channelsd picks max+1")
	require.Equal(t, 2, runnerIdx, "the runner independently picks the same max+1")

	// channelsd writes the human reply ("open a 5th PR") at index 2.
	require.NoError(t, a.Append(ctx, textTurn(channelsdIdx, "user", "open a 5th PR")))

	// The runner appends its tool_result user turn at the same index —
	// loop.go wraps this as "append user turn: %v" → MemoryUnavailable.
	err := a.Append(ctx, textTurn(runnerIdx, "user", "tool_result blocks"))
	require.ErrorIs(t, err, memory.ErrIndexConflict,
		"two concurrent user-turn writers picking the same max+1 index collide — the MemoryUnavailable bug")
}

// TestAppender_AuthorRoundTrip proves the per-turn Author subject
// round-trips through the signed turnContent payload, and that a
// different Author at the same (index, role) is a genuine conflict —
// not silently ignored.
func TestAppender_AuthorRoundTrip(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	in := memory.Turn{
		Index: 1, Role: "user",
		Content: []memory.ContentBlock{{Type: "text", Text: "hi"}},
		Author:  identity.Subject("user:YWxpY2U"),
	}
	require.NoError(t, a.Append(ctx, in))

	got, err := a.ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, identity.Subject("user:YWxpY2U"), got[0].Author)

	// Idempotent re-append of the identical turn (author included) is a no-op.
	require.NoError(t, a.Append(ctx, in))

	// A different author at the same (index, role) is a genuine conflict.
	conflict := in
	conflict.Author = identity.Subject("user:Ym9i")
	assert.ErrorIs(t, a.Append(ctx, conflict), memory.ErrIndexConflict)
}

// TestAppender_ViaRoundTrip proves the per-turn Via view-URN (identifying the
// browser/TUI surface that injected the turn) round-trips through the signed
// turnContent payload, same as Author.
func TestAppender_ViaRoundTrip(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	in := memory.Turn{
		Index: 3, Role: "user",
		Content: []memory.ContentBlock{{Type: "text", Text: "the CTA padding is wrong"}},
		Author:  identity.Subject("user:YWxpY2U"),
		Via:     "urn:ap:view:artifact:artifact-3f2a1b8c/artrev-9d4e0117",
	}
	require.NoError(t, a.Append(ctx, in))

	got, err := a.ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, in.Via, got[0].Via)
	assert.Equal(t, in.Author, got[0].Author)
}

// TestAppender_RefusedRoundTrip proves the per-turn Refused marker (set by
// the runner loop when a provider response carries stop_reason=refusal)
// round-trips through the stored turnContent payload, same as Via/Author.
// This guards against the field being added to memory.Turn but never wired
// into turn.Appender's (de)serialization — the exact gap that silently
// defeated replay()'s refused-turn skip logic until caught here.
func TestAppender_RefusedRoundTrip(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	in := memory.Turn{
		Index:   1,
		Role:    "assistant",
		Content: []memory.ContentBlock{{Type: "text", Text: "I cannot help with that."}},
		Refused: true,
	}
	require.NoError(t, a.Append(ctx, in))

	got, err := a.ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].Refused, "Refused must round-trip through Append/ReadAll")

	// Idempotent re-append of the identical turn (Refused included) is a no-op.
	require.NoError(t, a.Append(ctx, in))

	// A different Refused value at the same (index, role) is a genuine conflict.
	conflict := in
	conflict.Refused = false
	assert.ErrorIs(t, a.Append(ctx, conflict), memory.ErrIndexConflict)
}

// TestTurnWithoutViaOmitsTheField proves a turn with no Via serializes to
// stored content with no "via" key at all — not an empty-string "via":"".
// EntryDigest hashes the stored content JSON verbatim (canonicalContent), so
// an absent omitempty field is what keeps every pre-existing provenance
// chain's digest unchanged now that Via exists on the struct.
func TestTurnWithoutViaOmitsTheField(t *testing.T) {
	scope := memory.Scope{Kind: "session", ID: "default/s1"}
	mem := memory.NewLocal(inmem.NewBackend())
	a := turn.NewAppender(mem, scope)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, a.Append(ctx, memory.Turn{
		Index: 1, Role: "user",
		Content: []memory.ContentBlock{{Type: "text", Text: "hi"}},
	}))

	res, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{turn.KindName}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.NotContains(t, string(res.Entries[0].Content), `"via"`,
		"a turn with no Via must not emit a via key, or old-entry digests change")
	assert.NotContains(t, string(res.Entries[0].Content), `"refused"`,
		"a turn with Refused=false must not emit a refused key, or old-entry digests change")
	assert.NotContains(t, string(res.Entries[0].Content), `"model"`,
		"a turn with no Model must not emit a model key, or old-entry digests change")
}

// TestAppender_ModelRoundTrip proves the per-turn served-model display
// string (Task 7: "<provider>/<served-model>", e.g.
// "openrouter/anthropic/claude-3.5-sonnet") round-trips through the signed
// turnContent payload, same as Via/Refused. This guards against the field
// being added to memory.Turn but never wired into turn.Appender's
// (de)serialization — the exact gap the repo's accessor.go gotcha warns
// about.
func TestAppender_ModelRoundTrip(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	in := memory.Turn{
		Index:   1,
		Role:    "assistant",
		Content: []memory.ContentBlock{{Type: "text", Text: "hi there"}},
		Model:   "openrouter/anthropic/claude-3.5-sonnet",
	}
	require.NoError(t, a.Append(ctx, in))

	got, err := a.ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "openrouter/anthropic/claude-3.5-sonnet", got[0].Model,
		"Model must round-trip through Append/ReadAll")

	// Idempotent re-append of the identical turn (Model included) is a no-op.
	require.NoError(t, a.Append(ctx, in))

	// A different Model at the same (index, role) is a genuine conflict.
	conflict := in
	conflict.Model = "anthropic/claude-opus-4-8"
	assert.ErrorIs(t, a.Append(ctx, conflict), memory.ErrIndexConflict)
}

// TestAppender_OldEntryToleratesMissingModel proves an entry stored before
// the Model field existed (no "model" key in the stored JSON) still reads
// back cleanly with Model == "", rather than erroring.
func TestAppender_OldEntryToleratesMissingModel(t *testing.T) {
	scope := memory.Scope{Kind: "session", ID: "default/s1"}
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Simulate a pre-existing entry written before Model existed: no "model"
	// key at all in the stored content.
	_, err := mem.Put(ctx, memory.Entry{
		Scope: scope, Kind: turn.KindName, ID: turn.EntryID(1, "assistant"),
		CreatedAt: time.Unix(1, 0).UTC(),
		Content:   []byte(`{"content":[{"type":"text","text":"legacy turn"}]}`),
	})
	require.NoError(t, err)

	got, err := turn.ReadAll(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Model, "a pre-existing entry with no model key must read back as empty, not error")
}

// TestAppender_InboxRoleDoesNotCollideWithUserTurn proves the fix for the
// two-writer race documented by TestAppender_TwoWriterRace_IndexConflict.
//
// The turn Kind keys entries on (Index, Role). channelsd now writes
// inbound human messages under the distinct "inbox" role
// (internal/cmd/channelsd/memory.go's Append), while the runner remains the sole
// writer of "user"/"assistant" transcript turns. Because the role
// differs, a channelsd "inbox"-role append and a runner "user"-role
// append at the SAME numeric Index do NOT share an (Index, Role) key —
// both Appends succeed. The runner later drains the "inbox" turn into a
// real "user" turn at an index it controls (loop.drainInbox).
func TestAppender_InboxRoleDoesNotCollideWithUserTurn(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Prior conversation: user(0), assistant(1) — the assistant issued a
	// tool_use, so the runner still owes a tool_result "user" turn.
	require.NoError(t, a.Append(ctx, textTurn(0, "user", "do the thing")))
	require.NoError(t, a.Append(ctx, textTurn(1, "assistant", "calling a tool")))

	// channelsd writes the inbound human reply at max(Index)+1 == 2, but
	// under the distinct "inbox" role.
	require.NoError(t, a.Append(ctx, textTurn(2, "inbox", "also open a 5th PR")),
		"channelsd inbox-role append at index 2 must succeed")

	// The runner independently appends its tool_result "user" turn at the
	// SAME numeric index 2. The distinct role means no (Index, Role)
	// collision — this must NOT return ErrIndexConflict.
	err := a.Append(ctx, textTurn(2, "user", "tool_result blocks"))
	require.NoError(t, err,
		"runner user-turn at the same Index as a channelsd inbox turn must not conflict — the distinct role removes the collision")

	got, err := a.ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 4, "all four turns coexist")

	byRole := map[string]int{}
	for _, tn := range got {
		byRole[tn.Role]++
	}
	assert.Equal(t, 2, byRole["user"], "user(0) + runner tool_result user(2)")
	assert.Equal(t, 1, byRole["assistant"], "assistant(1)")
	assert.Equal(t, 1, byRole["inbox"], "channelsd inbox(2)")
}

// TestAppender_AttachmentContentRoundTrip proves a content block with
// Type=="attachment" and a populated AttachmentBlock survives Append/ReadAll
// unchanged, same as the ToolUse/ToolResult block shapes it sits alongside.
// Unlike Via/Refused/Model — direct memory.Turn fields that need explicit
// wiring into turnContent's marshal/unmarshal (accessor.go's three-place
// rule) — ContentBlock is serialized generically as part of Turn.Content, so
// a new ContentBlock field needs no separate accessor.go wiring; this test
// is the evidence for that, not an assumption.
func TestAppender_AttachmentContentRoundTrip(t *testing.T) {
	a := newAppender(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	in := memory.Turn{
		Index: 0, Role: "inbox",
		Content: []memory.ContentBlock{
			{Type: "text", Text: "see attached"},
			{Type: "attachment", Attachment: &memory.AttachmentBlock{
				Filename: "report.pdf", MIME: "application/pdf", SizeBytes: 4096,
				Ref:     "mem://ns/sess/inbound-asset/abc/raw/report.pdf",
				TextRef: "mem://ns/sess/inbound-asset/abc/text.txt", Pages: 3,
			}},
		},
	}
	require.NoError(t, a.Append(ctx, in))

	got, err := a.ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Content, 2)

	assert.Equal(t, "text", got[0].Content[0].Type)
	assert.Nil(t, got[0].Content[0].Attachment, "the text block must not pick up an Attachment")

	block := got[0].Content[1]
	require.Equal(t, "attachment", block.Type)
	require.NotNil(t, block.Attachment, "Attachment must round-trip through Append/ReadAll")
	assert.Equal(t, in.Content[1].Attachment.Filename, block.Attachment.Filename)
	assert.Equal(t, in.Content[1].Attachment.MIME, block.Attachment.MIME)
	assert.Equal(t, in.Content[1].Attachment.SizeBytes, block.Attachment.SizeBytes)
	assert.Equal(t, in.Content[1].Attachment.Ref, block.Attachment.Ref)
	assert.Equal(t, in.Content[1].Attachment.TextRef, block.Attachment.TextRef)
	assert.Equal(t, in.Content[1].Attachment.Pages, block.Attachment.Pages)
}
