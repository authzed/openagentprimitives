package runner_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
)

func tagScope(id string) memory.Scope { return memory.Scope{Kind: "session", ID: id} }

// putTag writes one pt_tag record the way the minter does.
func putTag(t *testing.T, m memory.Memory, scope memory.Scope, readers []string, toolUseID string) string {
	t.Helper()
	return putTagFrom(t, m, scope, readers, toolUseID)
}

// putTagFrom is putTag with the leaf's SOURCES named — the "<type>:<id>" of
// each object whose audience the tag was derived from, exactly as the minter
// records them. A pool read mints one of these per pool, all under one
// tool_use.
func putTagFrom(t *testing.T, m memory.Memory, scope memory.Scope, readers []string, toolUseID string, sources ...string) string {
	t.Helper()
	id := provenance.TagID(memory.NewID(pttag.Kind{}))
	leaf, err := provenance.NewLeaf(id, readers, false)
	require.NoError(t, err)
	content, err := json.Marshal(pttag.FromTag(leaf, sources, toolUseID, time.Now().UTC()))
	require.NoError(t, err)
	_, err = m.Put(memory.WithSystemApproval(context.Background(), "test"), memory.Entry{
		Scope: scope, Kind: pttag.KindName, ID: string(id), Content: content,
	})
	require.NoError(t, err)
	return string(id)
}

// TestResolvesTheTagMintedForThatCall — the happy path the handoff rests on.
func TestResolvesTheTagMintedForThatCall(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	scope := tagScope("demo/parent")
	want := putTag(t, m, scope, []string{"user:a"}, "toolu_7")
	putTag(t, m, scope, []string{"user:b"}, "toolu_9")

	got, err := runner.ResolveDataTagFor(m, scope)(context.Background(), "toolu_7")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestACallThatMintedNothingResolvesEmptyWithoutError.
//
// A tool that declared no read has no provenance to hand over. That is an
// ANSWER, not a failure — the caller turns it into a refusal the model can act
// on ("summarize it in the task text instead"), which is a different message
// from the one an outage produces.
func TestACallThatMintedNothingResolvesEmptyWithoutError(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	scope := tagScope("demo/parent")
	putTag(t, m, scope, []string{"user:a"}, "toolu_7")

	got, err := runner.ResolveDataTagFor(m, scope)(context.Background(), "toolu_nope")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestAnotherSessionsTagIsNotResolvable is the confinement property.
//
// The resolver is scoped to ONE session's records, which is what makes
// "the model names a call" safe: a tool_use_id borrowed from anywhere else
// resolves to nothing, so a parent cannot reach data it did not produce even
// if it guesses a real id.
func TestAnotherSessionsTagIsNotResolvable(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	mine, theirs := tagScope("demo/parent"), tagScope("demo/someone-else")
	putTag(t, m, theirs, []string{"user:a"}, "toolu_7")

	got, err := runner.ResolveDataTagFor(m, mine)(context.Background(), "toolu_7")
	require.NoError(t, err)
	assert.Empty(t, got, "a tag minted by another session must not be bindable from here")
}

// TestAnEmptyToolUseIdResolvesEmpty rather than matching a record that also
// happens to carry no tool use — a tag minted outside a tool call must not
// become bindable by sending an empty string.
func TestAnEmptyToolUseIdResolvesEmpty(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	scope := tagScope("demo/parent")
	putTag(t, m, scope, []string{"user:a"}, "")

	got, err := runner.ResolveDataTagFor(m, scope)(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestACallThatMintedSeveralTagsRefusesRatherThanPickingOne.
//
// One tool_use no longer means one tag. A memory search spanning resource
// pools mints one tag per POOL under a single search_memory tool_use, each
// governing only that pool's slice of the result — so "the tag for that call"
// stopped being a question with one answer.
//
// Returning the first match would bind the slot to one arbitrary pool and hand
// the child that pool's entries while dropping the others', with nothing said
// to anyone. Not a disclosure (tag and content are minted together, so the
// audience always matches the bytes), but silent data loss in a hand-off whose
// entire job is to be explicit about what is being handed over. Refusing is
// what the two callers already know how to render.
func TestACallThatMintedSeveralTagsRefusesRatherThanPickingOne(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	scope := tagScope("demo/parent")
	putTagFrom(t, m, scope, []string{"user:a"}, "toolu_pool", "customer:alpha")
	putTagFrom(t, m, scope, []string{"user:b"}, "toolu_pool", "vendor:beta")
	// A second call's tag, to pin that the refusal is about the ONE tool_use
	// named and not about the session having several tags.
	want := putTagFrom(t, m, scope, []string{"user:a"}, "toolu_single", "customer:alpha")

	resolve := runner.ResolveDataTagFor(m, scope)

	got, err := resolve(context.Background(), "toolu_pool")
	require.Error(t, err, "a call that read several resources has no single datum to hand over")
	assert.Empty(t, got, "and nothing is bound when the answer is ambiguous")
	assert.Contains(t, err.Error(), "customer:alpha", "the refusal must name what the call read")
	assert.Contains(t, err.Error(), "vendor:beta")

	single, err := resolve(context.Background(), "toolu_single")
	require.NoError(t, err, "a single-resource call is unaffected")
	assert.Equal(t, want, single)
}

// The refusal reaches the model through the two callers, so it has to READ as
// an instruction rather than as an internal error. Both prefix it ("could not
// look up the data for input %q: %v"), which is why the sentence has to carry
// the reason and the next move on its own.
func TestTheMultiTagRefusalTellsTheModelWhatToDoInstead(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	scope := tagScope("demo/parent")
	putTagFrom(t, m, scope, []string{"user:a"}, "toolu_pool", "customer:alpha")
	putTagFrom(t, m, scope, []string{"user:b"}, "toolu_pool", "vendor:beta")

	_, err := runner.ResolveDataTagFor(m, scope)(context.Background(), "toolu_pool")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "toolu_pool", "name the call, since the model named it")
	assert.Contains(t, err.Error(), "task text",
		"point at the move that works, as every other refusal on this path does")
}

// Sources are what the refusal names, and a tag can have none (a derived tag
// carries its derivation instead). The message must still say which call and
// how many, rather than rendering an empty list.
func TestTheMultiTagRefusalSurvivesTagsWithNoSources(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	scope := tagScope("demo/parent")
	putTag(t, m, scope, []string{"user:a"}, "toolu_pool")
	putTag(t, m, scope, []string{"user:b"}, "toolu_pool")

	_, err := runner.ResolveDataTagFor(m, scope)(context.Background(), "toolu_pool")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2", "the count is the fact that survives a missing source list")
}
