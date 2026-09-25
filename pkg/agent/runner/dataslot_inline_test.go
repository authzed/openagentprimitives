package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
)

// putContent stores one datum behind a tag, the way the minter does.
func putContent(t *testing.T, m memory.Memory, scope memory.Scope, tagID, content string) {
	t.Helper()
	body, err := json.Marshal(pttagcontent.ContentRecord{
		TagID: tagID, Content: content, MIME: "application/json", StoredAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	_, err = m.Put(memory.WithSystemApproval(context.Background(), "test"), memory.Entry{
		Scope: scope, Kind: pttagcontent.KindName,
		ID: memory.NewID(pttagcontent.Kind{}), Content: body,
	})
	require.NoError(t, err)
}

func bindings(pairs ...string) func(context.Context) ([]authz.DataSlotBinding, error) {
	return func(context.Context) ([]authz.DataSlotBinding, error) {
		out := make([]authz.DataSlotBinding, 0, len(pairs)/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, authz.DataSlotBinding{Slot: pairs[i], TagID: pairs[i+1]})
		}
		return out, nil
	}
}

// resolveFrom adapts a memory store to the resolve func ResolveBoundSlotsFor now
// takes — the operator's _pttag_resolve route in-process. Entitlement is always
// true here: confinement in these tests is that ResolveBoundSlotsFor only ASKS
// for bound tags, so an unbound tag in the same scope is never requested; the
// pt_tag#access gate is exercised at the operator handler and e2e harness.
func resolveFrom(m memory.Memory) func(context.Context, memory.Scope, []string) ([]memory.PtTagContent, error) {
	return func(ctx context.Context, scope memory.Scope, tagIDs []string) ([]memory.PtTagContent, error) {
		recs, err := pttagcontent.List(memory.WithSystemApproval(ctx, "test"), m, scope)
		if err != nil {
			return nil, err
		}
		return pttagcontent.ResolveEntitled(recs, tagIDs, func(string) (bool, error) { return true, nil })
	}
}

// TestBoundSlotsResolveToTheParentsStoredContent — the handoff's payoff. The
// parent minted the datum in ITS scope; the child reads it from there, because
// that is where the bytes are.
func TestBoundSlotsResolveToTheParentsStoredContent(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	parent := tagScope("demo/parent")
	putContent(t, m, parent, "ptt-a", `{"diff":"one line"}`)

	got, err := runner.ResolveBoundSlotsFor(resolveFrom(m), parent, bindings("diff", "ptt-a"))(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "diff", got[0].Slot)
	assert.Equal(t, "ptt-a", got[0].TagID)
	assert.Equal(t, `{"diff":"one line"}`, got[0].Content)
}

// TestOnlyBoundTagsAreReadable is the confinement property.
//
// The parent's scope holds every datum it ever minted. What the child may see
// is the set the OPERATOR bound onto it, after attenuation and grading — so an
// unbound tag sitting in the same scope must not come back, however well-known
// its id.
func TestOnlyBoundTagsAreReadable(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	parent := tagScope("demo/parent")
	putContent(t, m, parent, "ptt-bound", "handed over")
	putContent(t, m, parent, "ptt-secret", "never bound to this child")

	got, err := runner.ResolveBoundSlotsFor(resolveFrom(m), parent, bindings("diff", "ptt-bound"))(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "handed over", got[0].Content)
	for _, s := range got {
		assert.NotEqual(t, "never bound to this child", s.Content)
	}
}

// TestAMissingDatumIsReportedNotDropped.
//
// The child was told it has this slot. Omitting it silently would leave the
// model believing its parent withheld the datum — a different fact from "the
// datum was not stored", and one it would reason about differently.
func TestAMissingDatumIsReportedNotDropped(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	parent := tagScope("demo/parent")

	got, err := runner.ResolveBoundSlotsFor(resolveFrom(m), parent, bindings("diff", "ptt-gone"))(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1, "the slot must still be reported")
	assert.Empty(t, got[0].Content)

	blocks := runner.SlotContentBlocks(got)
	require.Len(t, blocks, 1)
	assert.Contains(t, blocks[0].Text, "unavailable",
		"the model must be told the slot is empty rather than left to infer it")
}

// TestAFailedListIsAnErrorNotAnEmptyHandoff: starting a child with no inputs
// because the lookup failed runs a DIFFERENT task from the one delegated, and
// looks to the model like a parent that chose to send nothing.
func TestAFailedListIsAnErrorNotAnEmptyHandoff(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	fail := func(context.Context) ([]authz.DataSlotBinding, error) {
		return nil, errors.New("spicedb unavailable")
	}

	_, err := runner.ResolveBoundSlotsFor(resolveFrom(m), tagScope("demo/parent"), fail)(context.Background())
	require.Error(t, err)
}

// TestASessionWithNoBoundSlotsResolvesToNothing — the 99% case. A session
// nobody delegated to must not pay for this path, and must not error on it.
func TestASessionWithNoBoundSlotsResolvesToNothing(t *testing.T) {
	m := memory.NewLocal(memoryinmem.NewBackend())
	none := func(context.Context) ([]authz.DataSlotBinding, error) { return nil, nil }

	got, err := runner.ResolveBoundSlotsFor(resolveFrom(m), tagScope("demo/parent"), none)(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestSlotBlocksNameTheirProvenance.
//
// A datum dropped into context with no framing reads as something the model
// established itself. A child that believes it verified a fact it was merely
// handed is the confusion this whole by-reference path exists to avoid, so the
// block says where the content came from.
func TestSlotBlocksNameTheirProvenance(t *testing.T) {
	blocks := runner.SlotContentBlocks([]runner.SlotDatum{
		{Slot: "diff", TagID: "ptt-a", Content: "the diff body"},
	})
	require.Len(t, blocks, 1)
	assert.Contains(t, blocks[0].Text, "diff", "the slot must be named")
	assert.Contains(t, blocks[0].Text, "delegated", "the source must be stated")
	assert.Contains(t, blocks[0].Text, "the diff body")
}
