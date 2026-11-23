package parkedprompt_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
)

func newMem(t *testing.T) memory.Memory {
	t.Helper()
	return memory.NewLocal(inmem.NewBackend())
}

func testCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "test")
}

var scope = memory.Scope{Kind: "AgentSession", ID: "ns/sess"}

func prompt(ref string) parkedprompt.Content {
	return parkedprompt.Content{
		RequestRef: ref,
		Category:   "identity_choice",
		Envelope:   []byte(`{"kind":"interaction_request"}`),
	}
}

// TestNote_SurvivesReadFromAFreshClient is the durability property the whole
// kind exists for: what was written is readable by a caller holding nothing
// from the writing process — which is what a channelsd restart leaves behind.
func TestNote_SurvivesReadFromAFreshClient(t *testing.T) {
	ctx, mem := testCtx(), newMem(t)

	require.NoError(t, parkedprompt.Note(ctx, mem, scope, prompt("req-1")))

	got, err := parkedprompt.Outstanding(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "req-1", got[0].RequestRef)
	assert.Equal(t, "identity_choice", got[0].Category)
	assert.JSONEq(t, `{"kind":"interaction_request"}`, string(got[0].Envelope),
		"the envelope must round-trip byte-identically so a replay matches the original delivery")
}

func TestNote_IsIdempotentPerRequestRef(t *testing.T) {
	ctx, mem := testCtx(), newMem(t)

	require.NoError(t, parkedprompt.Note(ctx, mem, scope, prompt("req-1")))
	require.NoError(t, parkedprompt.Note(ctx, mem, scope, prompt("req-1")))

	got, err := parkedprompt.Outstanding(ctx, mem, scope)
	require.NoError(t, err)
	assert.Len(t, got, 1, "re-noting the same requestRef must not duplicate it")
}

func TestNote_RequiresRequestRef(t *testing.T) {
	ctx, mem := testCtx(), newMem(t)
	err := parkedprompt.Note(ctx, mem, scope, parkedprompt.Content{Category: "identity_choice"})
	require.Error(t, err, "a prompt with no correlation key could never be resolved")
}

// TestResolve_StopsItBeingResurfaced: without this a decided prompt would be
// re-posted to every surface that attaches, forever.
func TestResolve_StopsItBeingResurfaced(t *testing.T) {
	ctx, mem := testCtx(), newMem(t)
	require.NoError(t, parkedprompt.Note(ctx, mem, scope, prompt("req-1")))
	require.NoError(t, parkedprompt.Note(ctx, mem, scope, prompt("req-2")))

	require.NoError(t, parkedprompt.Resolve(ctx, mem, scope, "req-1"))

	got, err := parkedprompt.Outstanding(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "req-2", got[0].RequestRef, "only the unresolved prompt remains outstanding")
}

// TestResolve_IsIdempotentAndTolerantOfUnknownRefs: the decision path and the
// gate-side timeout path both resolve, and either may run first. Neither may
// error on the other's leftovers.
func TestResolve_IsIdempotentAndTolerantOfUnknownRefs(t *testing.T) {
	ctx, mem := testCtx(), newMem(t)
	require.NoError(t, parkedprompt.Note(ctx, mem, scope, prompt("req-1")))

	require.NoError(t, parkedprompt.Resolve(ctx, mem, scope, "req-1"))
	require.NoError(t, parkedprompt.Resolve(ctx, mem, scope, "req-1"), "second resolve is a no-op")
	require.NoError(t, parkedprompt.Resolve(ctx, mem, scope, "never-noted"),
		"a regenerate category is never noted, so resolving it must not error")
	require.NoError(t, parkedprompt.Resolve(ctx, mem, scope, ""))

	got, err := parkedprompt.Outstanding(ctx, mem, scope)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestNote_NeverUnresolvesAResolvedTombstone pins the invariant the decision
// pipe's cross-restart idempotency rests on. The prompt envelope is re-published
// on OUT after the decision has landed — by the resurface republish that read
// Outstanding before the tombstone was written, and by the runner's re-emit of
// its outstanding requests on restart — and the outbound relay notes every OUT
// interaction_request it sees. A re-Note that reset Resolved would hand the
// decision pipe a live record, and a second click would re-run the bound handler
// in full (for tool_approval: a second SpiceDB grant tuple).
//
// The record must SURVIVE as a tombstone, not vanish: the gate reads
// absent = never noted (not a verdict) vs Resolved = decided.
func TestNote_NeverUnresolvesAResolvedTombstone(t *testing.T) {
	ctx, mem := testCtx(), newMem(t)
	require.NoError(t, parkedprompt.Note(ctx, mem, scope, prompt("req-1")))
	require.NoError(t, parkedprompt.Resolve(ctx, mem, scope, "req-1"))

	require.NoError(t, parkedprompt.Note(ctx, mem, scope, prompt("req-1")),
		"re-noting a decided prompt is a normal republish, not an error")

	got, err := parkedprompt.Outstanding(ctx, mem, scope)
	require.NoError(t, err)
	assert.Empty(t, got, "a decided prompt must not become re-surfaceable again")

	rec := storedContent(t, ctx, mem, "req-1")
	require.NotNil(t, rec, "the tombstone must remain readable: absent reads as never-decided")
	assert.True(t, rec.Resolved, "the durable already-resolved verdict must survive a re-Note")
}

// storedContent reads the raw record for requestRef INCLUDING a resolved
// tombstone — the tri-state the decision pipe reads (absent / live / decided),
// which Outstanding deliberately cannot express.
func storedContent(t *testing.T, ctx context.Context, mem memory.Memory, requestRef string) *parkedprompt.Content {
	t.Helper()
	res, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{parkedprompt.KindName}})
	require.NoError(t, err)
	for _, e := range res.Entries {
		var c parkedprompt.Content
		require.NoError(t, json.Unmarshal(e.Content, &c))
		if c.RequestRef == requestRef {
			return &c
		}
	}
	return nil
}

// TestOutstanding_IsScopedToItsSession guards the leak that matters most here:
// one session's prompt must never be re-surfaced into another's conversation.
func TestOutstanding_IsScopedToItsSession(t *testing.T) {
	ctx, mem := testCtx(), newMem(t)
	other := memory.Scope{Kind: "AgentSession", ID: "ns/other"}

	require.NoError(t, parkedprompt.Note(ctx, mem, scope, prompt("mine")))
	require.NoError(t, parkedprompt.Note(ctx, mem, other, prompt("theirs")))

	got, err := parkedprompt.Outstanding(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "mine", got[0].RequestRef)
}

// TestOutstanding_EmptyScope: a session with nothing parked reads clean rather
// than erroring — the overwhelmingly common case on every resurface request.
func TestOutstanding_EmptyScope(t *testing.T) {
	ctx, mem := testCtx(), newMem(t)
	got, err := parkedprompt.Outstanding(ctx, mem, scope)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestKind_IsNotAppendOnly pins the retention choice that makes Resolve
// possible at all: the audit kinds refuse a content change, and declaring this
// one append-only would silently make every resolve fail and every decided
// prompt immortal.
func TestKind_IsNotAppendOnly(t *testing.T) {
	assert.False(t, parkedprompt.Kind{}.Retention().AppendOnly,
		"parked prompts are working state, not audit evidence — Resolve must be able to overwrite")
}

func TestConcurrentResolutionAndReparkKeepFirstOutcome(t *testing.T) {
	ctx, mem := testCtx(), newMem(t)
	original := prompt("concurrent")
	require.NoError(t, parkedprompt.Note(ctx, mem, scope, original))
	start := make(chan struct{})
	results := make(chan parkedprompt.Content, 32)
	errors := make(chan error, 64)
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(2)
		go func(i int) {
			defer workers.Done()
			<-start
			content, err := parkedprompt.ResolveWithOutcome(ctx, mem, scope, original.RequestRef, []byte(fmt.Sprintf("outcome-%d", i)))
			results <- content
			errors <- err
		}(i)
		go func() { defer workers.Done(); <-start; errors <- parkedprompt.Note(ctx, mem, scope, original) }()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	winner, found, err := parkedprompt.Find(ctx, mem, scope, original.RequestRef)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, winner.Resolved)
	require.NotEmpty(t, winner.Resolution)
	for result := range results {
		require.Equal(t, winner.Resolution, result.Resolution)
	}
}
